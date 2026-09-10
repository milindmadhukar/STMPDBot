package stmpdbot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgolink/v4/disgolink"
	"github.com/disgoorg/snowflake/v2"
	"github.com/golang-migrate/migrate/v4"
	migratePgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxStdlib "github.com/jackc/pgx/v5/stdlib"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/utils"
	"google.golang.org/api/youtube/v3"
	"gopkg.in/natefinch/lumberjack.v2"
)

func New(cfg Config, version string, commit string) *STMPDBot {
	return &STMPDBot{
		Cfg:       cfg,
		Version:   version,
		Commit:    commit,
		SingAlong: utils.NewSingAlongState(),
	}
}

type STMPDBot struct {
	Cfg     Config
	Client  *bot.Client
	Version string
	Commit  string
	IsReady bool

	DB             *pgxpool.Pool
	Queries        *db.Queries
	YoutubeService *youtube.Service

	RedditToken utils.RedditToken
	// RedditClient carries every Reddit call, and goes through bot.reddit_proxy
	// when one is set. See SetupReddit.
	RedditClient   *http.Client
	RadioManager   *utils.RadioManager
	BeatportClient *utils.BeatportClient
	// AgentClient is nil unless the AI persona feature is configured and
	// enabled -- see SetupLLM. It talks to the standalone agent service
	// (cmd/agent) over HTTP; the bot never holds an LLM API key itself.
	AgentClient *utils.AgentClient

	// SingAlong is the live sing-along round for every guild running one, so the
	// message listener can answer "is this a sing-along channel?" without a query
	// on every message in every guild. The database stays the authority; this is
	// rebuilt from it on the scheduler's first pass.
	SingAlong *utils.SingAlongState

	// SingAlongReroll re-rolls a guild's sing-along on demand, for the dashboard's
	// button. It is a hook rather than a direct call because the internal API lives
	// in this package and stmpdbot/handlers imports it, so the dependency can only
	// run one way; main.go wires it up. Nil until then, which the route reports as
	// unavailable rather than pretending to have worked.
	SingAlongReroll func(ctx context.Context, guildID snowflake.ID) (db.Song, error)

	// resolveCache memoises user lookups made by the internal API, so paging
	// through a log does not re-request the same moderators on every page.
	// See resolveUser in internalapi.go.
	resolveMu    sync.Mutex
	resolveCache map[string]resolvedUser
}

func (b *STMPDBot) SetupBot(listeners ...bot.EventListener) error {
	client, err := disgo.New(b.Cfg.Bot.Token,
		// IntentGuildModeration is what delivers GUILD_AUDIT_LOG_ENTRY_CREATE,
		// and with it every kick, ban and timeout performed through Discord's
		// own UI rather than through /moderation. Without it the moderation log
		// only ever shows what the bot itself did. It is not a privileged
		// intent, so it needs no Developer Portal change -- but the bot does
		// need the View Audit Log permission in the guild.
		bot.WithGatewayConfigOpts(gateway.WithIntents(gateway.IntentGuilds, gateway.IntentGuildMessages, gateway.IntentMessageContent, gateway.IntentGuildMembers, gateway.IntentGuildVoiceStates, gateway.IntentGuildModeration)),
		// FlagRoles and FlagChannels are what let the dashboard render role and
		// channel pickers without a REST call per page. IntentGuilds is already
		// enabled and already delivers both in GUILD_CREATE and keeps them
		// current via GUILD_ROLE_* / CHANNEL_*, so this costs two more cache
		// maps and no new intents -- the data was simply being discarded.
		// They also make disgo's MemberPermissions and
		// MemberPermissionsInChannel usable, which need FlagRoles and both
		// flags respectively.
		bot.WithCacheConfigOpts(cache.WithCaches(cache.FlagGuilds, cache.FlagMessages, cache.FlagVoiceStates, cache.FlagMembers, cache.FlagRoles, cache.FlagChannels)),
		bot.WithEventListeners(listeners...),
	)
	if err != nil {
		return err
	}

	b.Client = client

	return nil
}

// TODO: Make foreign key constraints on tables
func (b *STMPDBot) SetupDB() error {
	tries := 5
	DBConn, err := pgxpool.New(context.Background(), b.Cfg.DB.URI())
	if err != nil {
		return err
	}

	for tries > 0 {
		slog.Info("Attempting to make a connection to the database...")
		err = DBConn.Ping(context.Background())
		if err != nil {
			tries -= 1
			slog.Info(err.Error() + "\nCould not connect. Retrying...")
			time.Sleep(5 * time.Second)
			continue
		}
		b.Queries = db.New(DBConn)
		b.DB = DBConn
		slog.Info("Connection to the database established.")

		driver, err := migratePgx.WithInstance(
			pgxStdlib.OpenDBFromPool(DBConn),
			&migratePgx.Config{},
		)

		if err != nil {
			return err
		}

		m, err := migrate.NewWithDatabaseInstance(
			"file://db/migrations",
			"postgres", driver)

		if err != nil {
			return err
		}

		if err = m.Up(); err != nil {
			if errors.Is(err, migrate.ErrNoChange) {
				slog.Info("Database is already up to date.")
				return nil
			}

			return err
		}

		slog.Info("Database migrated to latest migration.")
		return nil
	}
	return errors.New("could not make a connection to the database")
}

// SetupBackgrounds points the rank card generator at the configured
// backgrounds directory and makes sure the built-in images are present on
// disk and in the catalogue. That is what makes a fresh deploy work: an empty
// volume and an empty backgrounds table still have something to render with,
// with no manual seeding step.
func (b *STMPDBot) SetupBackgrounds() error {
	dir := b.Cfg.Storage.BackgroundsDir
	if dir == "" {
		dir = "assets/backgrounds"
	}
	utils.SetBackgroundsDir(dir)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating backgrounds dir: %w", err)
	}

	// The bundled images baked into the bot's own image are always the seed
	// source, regardless of where the configured dir points.
	const seedDir = "assets/backgrounds"
	entries, err := os.ReadDir(seedDir)
	if err != nil {
		return fmt.Errorf("reading bundled backgrounds: %w", err)
	}

	// In local/dev, dir defaults to seedDir itself, so there is nothing to
	// copy -- only a production deploy pointing dir at a separate mounted
	// volume needs the files copied in.
	sameDir := filepath.Clean(dir) == filepath.Clean(seedDir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	seeded := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		if !sameDir {
			dest := filepath.Join(dir, name)
			if _, err := os.Stat(dest); os.IsNotExist(err) {
				if err := copyFile(filepath.Join(seedDir, name), dest); err != nil {
					return fmt.Errorf("seeding background %s: %w", name, err)
				}
			}
		}

		// ON CONFLICT (filename) DO UPDATE makes this idempotent -- safe to
		// run on every startup, not just the first one.
		if _, err := b.Queries.CreateBackground(ctx, db.CreateBackgroundParams{Filename: name}); err != nil {
			return fmt.Errorf("cataloguing background %s: %w", name, err)
		}
		seeded++
	}

	slog.Info("Backgrounds ready", slog.String("dir", dir), slog.Int("seeded", seeded))
	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// SetupBeatport initializes the Beatport API client
func (b *STMPDBot) SetupBeatport() error {
	if b.Cfg.Bot.BeatportUsername == "" || b.Cfg.Bot.BeatportPassword == "" {
		slog.Warn("Beatport credentials not configured, beatport features will be disabled")
		return nil
	}

	maxTracks := b.Cfg.Bot.BeatportMaxTracks
	if maxTracks == 0 {
		maxTracks = 50
	}

	config := &utils.BeatportConfig{
		Username:  b.Cfg.Bot.BeatportUsername,
		Password:  b.Cfg.Bot.BeatportPassword,
		LabelID:   b.Cfg.Bot.BeatportLabelID,
		ArtistIDs: b.Cfg.Bot.BeatportArtistIDs,
		MaxTracks: maxTracks,
	}

	client, err := utils.NewBeatportClient(config)
	if err != nil {
		return fmt.Errorf("failed to create beatport client: %w", err)
	}

	b.BeatportClient = client
	slog.Info("Beatport client initialized",
		slog.String("label_id", config.LabelID),
		slog.Int("artist_count", len(config.ArtistIDs)),
		slog.Int("max_tracks", maxTracks))
	return nil
}

// redditTimeout bounds every Reddit call. The fetcher loop is the only thing
// driving these requests, so a hung connection with no timeout would stall
// Reddit notifications indefinitely.
const redditTimeout = 30 * time.Second

// SetupReddit builds the client every Reddit call goes through. With
// bot.reddit_proxy set, Reddit is the only source that leaves through it;
// without one it is fetched directly, like everything else.
//
// A proxy that cannot be used is logged and skipped rather than disabling
// Reddit: the direct route is how it ran before the option existed, and a
// typo in the config should cost the proxy, not the notifications.
func (b *STMPDBot) SetupReddit() {
	client, err := utils.NewHTTPClient(redditTimeout, b.Cfg.Bot.RedditProxy)
	if err != nil {
		slog.Error("Unusable bot.reddit_proxy, fetching Reddit directly instead", slog.Any("err", err))
		client, _ = utils.NewHTTPClient(redditTimeout, "")
	}

	b.RedditClient = client
	slog.Info("Reddit client initialized", slog.Bool("via_proxy", client.Transport != nil))
}

// SetupLLM points the bot at the standalone AI persona service (cmd/agent).
// It follows SetupBeatport's shape on purpose: an unconfigured or disabled
// feature leaves b.AgentClient nil rather than erroring, and every call site
// guards on that nil rather than on a separate flag. The bot itself never
// holds an LLM API key -- that lives only in the agent service's own config.
func (b *STMPDBot) SetupLLM() error {
	if !b.Cfg.LLM.Enabled {
		slog.Warn("AI persona feature disabled (llm.enabled = false)")
		return nil
	}
	if b.Cfg.LLM.AgentURL == "" || b.Cfg.LLM.AgentSecret == "" {
		slog.Warn("AI persona feature not configured, mention/reply triggers will be disabled")
		return nil
	}

	b.AgentClient = utils.NewAgentClient(b.Cfg.LLM.AgentURL, b.Cfg.LLM.AgentSecret)
	slog.Info("AI persona agent client initialized", slog.String("agent_url", b.Cfg.LLM.AgentURL))
	return nil
}

func (b *STMPDBot) OnReady(e *events.Ready) {
	slog.Info("STMPD Bot ready")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	slog.Info("Bot Name: " + e.User.Username)
	slog.Info("Bot ID: " + e.User.ID.String())
	slog.Info(fmt.Sprintf("Total Guilds: %d", len(e.Guilds)))

	// Ensure all guilds have configurations (in case bot was added while offline)
	b.ensureGuildConfigurations(e.Guilds)

	// TODO: Update presence
	if err := b.Client.SetPresence(ctx, gateway.WithListeningActivity("you"), gateway.WithOnlineStatus(discord.OnlineStatusOnline)); err != nil {
		slog.Error("Failed to set presence", slog.Any("err", err))
	}

	b.IsReady = true
}

// ensureGuildConfigurations checks if configurations exist for all guilds and creates missing ones
func (b *STMPDBot) ensureGuildConfigurations(guilds []discord.UnavailableGuild) {
	slog.Info("Checking guild configurations...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	created := 0
	existing := 0
	failed := 0

	for _, guild := range guilds {
		guildID := int64(guild.ID)

		// Check if guild configuration exists
		_, err := b.Queries.GetGuild(ctx, guildID)
		if err != nil {
			// Guild configuration doesn't exist, create it
			_, createErr := b.Queries.CreateGuild(ctx, guildID)
			if createErr != nil {
				slog.Error("Failed to create guild configuration",
					slog.Any("guild_id", guild.ID),
					slog.Any("err", createErr))
				failed++
				continue
			}
			slog.Info("Created missing guild configuration", slog.Any("guild_id", guild.ID))
			created++
		} else {
			existing++
		}
	}

	slog.Info("Guild configuration check complete",
		slog.Int("total", len(guilds)),
		slog.Int("existing", existing),
		slog.Int("created", created),
		slog.Int("failed", failed))
}

func SetupLogger(cfg LogConfig) {
	// Set timezone from config, default to Asia/Kolkata if not specified
	timezone := cfg.TimeZone
	if timezone == "" {
		timezone = "Asia/Kolkata"
	}

	loc, err := time.LoadLocation(timezone)
	if err != nil {
		slog.Error("Failed to load timezone, falling back to Asia/Kolkata",
			slog.String("timezone", timezone),
			slog.Any("err", err))
		loc, err = time.LoadLocation("Asia/Kolkata")
	}

	// Only assign a location we actually resolved. Assigning the nil returned by a
	// failed lookup does not leave the previous value in place -- Go reads a nil
	// *Location as UTC -- so the old code turned a bad timezone into a silent switch
	// to UTC, which is exactly what happened in production.
	if err != nil || loc == nil {
		slog.Error("Could not resolve any timezone, keeping the process default",
			slog.String("default", time.Local.String()))
	} else {
		time.Local = loc
	}

	opts := &slog.HandlerOptions{
		AddSource: cfg.AddSource,
		Level:     cfg.Level,
	}

	fileWriter := &lumberjack.Logger{
		Filename:   cfg.File,
		MaxSize:    cfg.MaxSize,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAge,
		Compress:   true,
		LocalTime:  true, // Use local time for log rotation
	}

	multiWriter := io.MultiWriter(os.Stdout, fileWriter)

	var sHandler slog.Handler
	switch cfg.Format {
	case "json":
		sHandler = slog.NewJSONHandler(multiWriter, opts)
	case "text":
		sHandler = slog.NewTextHandler(multiWriter, opts)
	default:
		slog.Error("Unknown log format", slog.String("format", cfg.Format))
		os.Exit(-1)
	}

	slog.SetDefault(slog.New(sHandler))
}

// SetupLavalink initializes the Lavalink client and connects to the node
func (b *STMPDBot) SetupLavalink(ctx context.Context) error {
	b.RadioManager = utils.NewRadioManager(b.Client.ApplicationID)

	// Set up disconnect callback
	b.RadioManager.OnLavalinkDisconnect = func() {
		slog.Error("Lavalink permanently disconnected - disconnecting from all radio channels")
		disconnectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		b.DisconnectAllRadioChannels(disconnectCtx)
	}

	// Connect to Lavalink with retry logic
	if err := b.RadioManager.ConnectToLavalink(ctx, b.Cfg.Lavalink.URL, b.Cfg.Lavalink.Password); err != nil {
		return err
	}

	// Start monitoring Lavalink connection (max 10 reconnect attempts = ~50 seconds)
	go b.RadioManager.MonitorLavalinkConnection(10)

	return nil
}

// RegisterLavalinkListeners registers Lavalink event listeners
// This should be called from main.go after SetupLavalink to avoid import cycles
func (b *STMPDBot) RegisterLavalinkListeners(eventListeners ...disgolink.EventListener) {
	b.RadioManager.Client.AddListeners(eventListeners...)
}
