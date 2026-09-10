package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	// The runtime image is bare alpine, which ships no tzdata, so every
	// time.LoadLocation call failed with "unknown time zone Asia/Kolkata" -- and
	// SetupLogger's fallback failed the same way, leaving time.Local nil, which Go
	// treats as UTC. Embedding the database costs about 450KB and makes the bot's
	// timezone handling independent of what the base image happens to carry.
	_ "time/tzdata"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/snowflake/v2"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/stmpdbot/commands"
	"github.com/milindmadhukar/STMPDBot/stmpdbot/handlers"
	"github.com/milindmadhukar/STMPDBot/stmpdbot/listeners"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
)

var (
	Version string
	Commit  string
)

func main() {
	// Version/Commit are normally stamped in at build time via -ldflags; fall back
	// to the environment (and then to placeholders) for `go run` and local builds.
	if Version == "" {
		Version = os.Getenv("VERSION")
	}
	if Commit == "" {
		Commit = os.Getenv("COMMIT")
	}
	if Version == "" {
		Version = "dev"
	}
	if Commit == "" {
		Commit = "unknown"
	}

	shouldSyncCommands := flag.Bool("sync-commands", false, "Whether to sync commands to discord")
	path := flag.String("config", "config.toml", "path to config")
	shouldClearCommands := flag.Bool("clear-commands", false, "Whether to clear commands from discord")
	flag.Parse()

	cfg, err := stmpdbot.LoadConfig(*path)
	if err != nil {
		slog.Error("Failed to read config", slog.Any("err", err))
		os.Exit(-1)
	}

	stmpdbot.SetupLogger(cfg.Log)
	slog.Info("Starting STMPD Bot..", slog.String("version", Version), slog.String("commit", Commit))
	slog.Info("Syncing commands", slog.Bool("sync", *shouldSyncCommands))

	b := stmpdbot.New(*cfg, Version, Commit)

	// TODO: Disable app commands in DMs
	h := commands.SetupHandlers(b)

	if err = b.SetupDB(); err != nil {
		slog.Error("Failed to setup db", slog.Any("err", err))
		os.Exit(-1)
	}

	// Started after SetupDB because seeding writes through b.Queries.
	if err = b.SetupBackgrounds(); err != nil {
		slog.Error("Failed to setup rank card backgrounds", slog.Any("err", err))
		os.Exit(-1)
	}

	// Started after SetupDB so the database check has a pool, but before the
	// gateway opens so /health answers during startup (reporting unhealthy)
	// rather than refusing connections.
	b.StartHealthServer()

	// Exactly one credential, never both: the Google client rejects being handed an
	// API key and a service-account file together with "multiple credential options
	// provided". This went unnoticed for as long as BotConfig read the key from the
	// wrong TOML name, because the key was always empty and only the file was really
	// being passed.
	//
	// The API key is preferred. Everything the bot asks YouTube for is public, and a
	// key needs no account to be kept alive.
	var ytOption option.ClientOption
	switch {
	case b.Cfg.Bot.YoutubeAPIKey != "":
		ytOption = option.WithAPIKey(b.Cfg.Bot.YoutubeAPIKey)
	case b.Cfg.Bot.GoogleServiceFile != "":
		// WithCredentialsFile is deprecated: it accepts any credential type without
		// validation. The file here is always a service account key, so name that type.
		ytOption = option.WithAuthCredentialsFile(option.ServiceAccount, b.Cfg.Bot.GoogleServiceFile)
	default:
		slog.Error("No YouTube credentials configured: set yt_api_key or google_service_file")
		os.Exit(-1)
	}

	service, err := youtube.NewService(context.Background(), ytOption)
	if err != nil {
		slog.Error("Failed to create youtube service", slog.Any("err", err))
		os.Exit(-1)
	}
	b.YoutubeService = service

	if err = b.SetupBot(h,
		bot.NewListenerFunc(b.OnReady),
		listeners.VoiceStateUpdateListener(b),
		listeners.VoiceServerUpdateListener(b),
		listeners.MessageCreateListener(b),
		// Experimental: answers a mention or a reply to the bot with an LLM
		// response. Stays a no-op listener until b.SetupLLM() below sets
		// b.AIClient -- see stmpdbot/ai for how to remove this entirely.
		listeners.AIListener(b),
		listeners.GuildJoinListener(b),
		listeners.GuildMemberJoinListener(b),
		listeners.GuildMemberLeaveListener(b),
		listeners.MessageDeleteListener(b),
		listeners.MessageUpdateListener(b),
		// Records kicks, bans and timeouts performed through Discord's own UI
		// into the same modlogs table as the /moderation commands.
		listeners.GuildAuditLogListener(b),
		// Voice and profile changes relay to their configured channels and are
		// never persisted, so they add no tables and no retention problem.
		listeners.VoiceLogJoinListener(b),
		listeners.VoiceLogLeaveListener(b),
		listeners.VoiceLogMoveListener(b),
		listeners.GuildMemberProfileListener(b),
		// Registered after MessageCreateListener, which is what creates a member's
		// users row -- disgo dispatches listeners in order, so by the time a
		// sing-along award runs the row it pays into exists.
		listeners.SingAlongListener(b),
	); err != nil {
		slog.Error("Failed to setup bot", slog.Any("err", err))
		os.Exit(-1)
	}

	// Started after SetupBot because every handler reads b.Client's caches, and
	// before OpenGateway so a dashboard that boots first gets served (with empty
	// caches) rather than refused.
	b.StartInternalAPI()

	// Before the fetchers below start, which is the only thing that uses it.
	b.SetupReddit()

	// Setup Beatport client
	if err = b.SetupBeatport(); err != nil {
		slog.Warn("Failed to setup Beatport client - beatport features will be disabled", slog.Any("err", err))
	}

	// Experimental AI persona feature. See the stmpdbot/ai package doc
	// comment for how to remove it entirely once the trial is over.
	if err = b.SetupLLM(); err != nil {
		slog.Warn("Failed to setup AI persona client - mention/reply triggers will be disabled", slog.Any("err", err))
	}

	// Setup Lavalink (non-blocking, warnings only)
	if err = b.SetupLavalink(context.Background()); err != nil {
		slog.Warn("Failed to setup Lavalink - radio features will be disabled until connection is established", slog.Any("err", err))
		slog.Warn("You can use '/radio start' command to retry connection later")
	} else {
		// Register Lavalink event listeners only if connection succeeded
		b.RegisterLavalinkListeners(
			listeners.LavalinkTrackStartListener(b),
			listeners.LavalinkTrackEndListener(b),
			listeners.LavalinkTrackExceptionListener(b),
			listeners.LavalinkTrackStuckListener(b),
			listeners.LavalinkWebSocketClosedListener(b),
		)
	}

	// TODO: Seems out of place, place somwehere more appropriate
	go func() {
		for {
			if b.IsReady {
				go handlers.GetRedditPosts(b, time.NewTicker(3*time.Minute))
				go handlers.GetYoutubeVideos(b, time.NewTicker(3*time.Minute))
				go handlers.GetAllStmpdReleases(b, time.NewTicker(15*time.Minute))
				go handlers.GetBeatportReleases(b, time.NewTicker(15*time.Minute))
				go handlers.GetAllTourShows(b, time.NewTicker(10*time.Minute))
				// Fills gaps neither STMPD nor beatport can, a small batch at a
				// time, so the maintenance scripts stay one-off repairs.
				go handlers.GetSongEnrichment(b, time.NewTicker(1*time.Hour))

				// Daily rather than hourly: the backlog's retry floor is seven days,
				// so anything faster only re-runs a query that returns nothing. The
				// bulk of the catalogue is filled by scripts/backfill-lyrics; this
				// picks up what LRCLIB gains afterwards.
				go handlers.GetSongLyrics(b, time.NewTicker(24*time.Hour))

				// Adds link buttons to an already-posted release announcement as the
				// links are discovered. Hourly because that is the cadence of the
				// enrichment producing them; a cycle where nothing changed makes no
				// Discord calls at all.
				go handlers.RefreshAnnouncements(b, time.NewTicker(1*time.Hour))

				// Ticks far more often than it posts. Unlike the feeds above there
				// is no remote source to poll -- what it waits for is each guild's
				// own configured local hour, so the schedule lives per guild and
				// this is only how often that gets checked. The 5 minute poll is
				// also what lets a restart pick up a window it slept through.
				go handlers.GetSongAnniversaries(b, time.NewTicker(5*time.Minute))

				// Same shape and the same five minute poll as the anniversaries, and
				// for the same reasons: the schedule is each guild's own local hour,
				// and the poll is what lets a restart pick up a window it slept
				// through. It also reconciles the channel's slowmode with the
				// configured cooldown, which is how a dashboard edit reaches Discord.
				b.SingAlongReroll = handlers.RerollSingAlong(b)
				go handlers.RunSingAlong(b, time.NewTicker(5*time.Minute))

				// Repairs derived state that has drifted from what the current rules
				// produce -- rekey-songs and link-remix-parents, as a ticker.
				//
				// Six hourly because it is a whole-table read and the drift it fixes
				// arrives either one row at a time from the fetchers or all at once
				// when a normalisation rule changes, and neither is urgent. A cycle
				// with nothing stale writes nothing.
				//
				// It never deletes a row. Merging duplicates needs judgement about
				// which credit and which date survive, so that stays with dedupe-songs
				// and the dashboard.
				go handlers.SelfHealCatalogue(b, time.NewTicker(6*time.Hour))

				// Auto-start radio in all configured guilds (only if Lavalink is connected)
				go func() {
					time.Sleep(5 * time.Second) // Wait for everything to be ready

					// Only auto-start if Lavalink is connected
					if b.RadioManager == nil || !b.RadioManager.IsLavalinkConnected() {
						slog.Info("Lavalink not connected - skipping auto-start of radio")
						return
					}

					radioConfigs, err := b.Queries.GetRadioVoiceChannels(context.Background())
					if err != nil {
						slog.Error("Failed to get radio configurations", slog.Any("err", err))
						return
					}

					for _, config := range radioConfigs {
						if config.RadioVoiceChannel.Valid {
							guildID := snowflake.ID(config.GuildID)
							slog.Info("Auto-starting radio", slog.String("guild_id", guildID.String()))
							if err := b.StartRadioInGuild(context.Background(), guildID); err != nil {
								slog.Error("Failed to start radio", slog.Any("err", err), slog.String("guild_id", guildID.String()))
							}
						}
					}
				}()

				return
			}
			time.Sleep(1 * time.Second)
		}
	}()

	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		b.Client.Close(ctx)
	}()

	if *shouldSyncCommands {
		// dev_guilds was being ignored: this passed a hardcoded nil, so the config
		// option documented as "add guild ids the commands should sync to, leave
		// empty to sync globally" did nothing at all.
		//
		// It is worth honouring rather than deleting -- guild commands appear
		// instantly where a global sync can take an hour to propagate, which is the
		// whole point of the option while developing.
		//
		// Note that SyncCommands does not clear the global set when given guild ids.
		// A deployment that sets dev_guilds therefore freezes whatever is registered
		// globally, and every guild but those listed keeps answering from it. That is
		// a development convenience; production must leave dev_guilds empty.
		guilds := b.Cfg.Bot.DevGuilds
		if len(guilds) == 0 {
			slog.Info("Syncing commands globally")
		} else {
			slog.Warn("Syncing commands to specific guilds; the global set is left as it is",
				slog.Any("dev_guilds", guilds))
		}
		if err = handler.SyncCommands(b.Client, commands.Commands, guilds); err != nil {
			slog.Error("Failed to sync commands", slog.Any("err", err))
		}
	}

	if *shouldClearCommands {
		slog.Info("Clearing all commands")
		if err = handler.SyncCommands(b.Client, nil, nil); err != nil {
			slog.Error("Failed to clear commands", slog.Any("err", err))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err = b.Client.OpenGateway(ctx); err != nil {
		slog.Error("Failed to open gateway", slog.Any("err", err))
		os.Exit(-1)
	}

	slog.Info("Bot is running. Press CTRL-C to exit.")
	s := make(chan os.Signal, 1)
	signal.Notify(s, syscall.SIGINT, syscall.SIGTERM)
	<-s
	slog.Info("Shutting down bot...")

	// Graceful shutdown: disconnect from all radio channels and clear status
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if b.RadioManager != nil {
		slog.Info("Disconnecting from all radio channels...")
		b.DisconnectAllRadioChannels(shutdownCtx)
	}
}
