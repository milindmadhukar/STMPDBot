package stmpdbot

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/disgoorg/snowflake/v2"
	"github.com/pelletier/go-toml/v2"
)

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open config: %w", err)
	}

	var cfg Config
	if err = toml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	warnUnknownKeys(data)
	return &cfg, nil
}

// warnUnknownKeys logs every key in the file that no field of Config claims.
// They are ignored either way -- this only makes the ignoring audible.
//
// It is a warning and not an error on purpose: a config that has drifted
// ahead of the binary must still boot, which is the whole reason one file is
// shared between the bot, the dashboard and the agent.
//
// Worth having because the failure is otherwise completely silent, and has
// bitten twice. Once when a struct tag said "youtube_api_key" while every
// config wrote "yt_api_key" (see the test in config_test.go). Once when
// splitting the AI feature into cmd/agent moved base_url/api_key/model out of
// [llm] into [agent] and the deployed config kept its copies -- so the LLM API
// key sat in a section the bot reads, in a file the bot and dashboard
// containers both mount, for as long as nobody looked.
func warnUnknownKeys(data []byte) {
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var probe Config
	var strictErr *toml.StrictMissingError
	if err := dec.Decode(&probe); errors.As(err, &strictErr) {
		for _, e := range strictErr.Errors {
			// The key only -- never the value, which is how half of these
			// end up being secrets.
			slog.Warn("Config key is not read by anything and will be ignored",
				slog.String("key", strings.Join(e.Key(), ".")))
		}
	}
}

type Config struct {
	Log      LogConfig      `toml:"log"`
	Bot      BotConfig      `toml:"bot"`
	Lavalink LavalinkConfig `toml:"lavalink"`
	DB       DatabaseConfig `toml:"database"`
	Health   HealthConfig   `toml:"health"`
	// Internal is read by the bot, Dashboard by the dashboard binary. Both
	// live in the same file so there is one config format and one
	// LoadConfig; each process simply ignores the section it does not own.
	Internal  InternalConfig  `toml:"internal"`
	Dashboard DashboardConfig `toml:"dashboard"`
	// Storage is read by both binaries: the bot renders rank cards from it,
	// the dashboard serves thumbnails and writes new uploads into it.
	Storage StorageConfig `toml:"storage"`
	// LLM is read by the bot: whether/how to forward a triggered mention or
	// reply to the standalone agent service. See SetupLLM.
	LLM LLMConfig `toml:"llm"`
	// Agent is read only by cmd/agent -- the standalone AI persona service.
	// It lives in the same file as everything else here for the same reason
	// Dashboard does: one config format, one LoadConfig, each process
	// ignores the sections it doesn't own.
	Agent AgentConfig `toml:"agent"`
}

// LLMConfig controls the bot's side of the AI persona feature: whether the
// trigger listener is active, and how to reach the standalone agent service
// that actually talks to the LLM. It is deliberately kept to one small
// section so the trigger can be removed by deleting this struct, the two
// call sites in main.go, and stmpdbot/listeners/ai.go.
type LLMConfig struct {
	// Enabled is an explicit pause switch, independent of whether AgentURL is
	// set -- so the feature can be turned off instantly without touching
	// anything else. SetupLLM also leaves the client nil when AgentURL or
	// AgentSecret is empty, mirroring SetupBeatport.
	Enabled bool `toml:"enabled"`
	// AgentURL points at cmd/agent's Address, e.g. "http://agent:8083".
	AgentURL string `toml:"agent_url"`
	// AgentSecret must equal Agent.Secret below.
	AgentSecret string `toml:"agent_secret"`
	// MaxContextMessages bounds how far up the Discord reply chain a
	// triggered response walks for context.
	MaxContextMessages int `toml:"max_context_messages"`
	// CooldownSeconds is the minimum gap between two triggered responses to
	// the same user, enforced in memory.
	CooldownSeconds int `toml:"cooldown_seconds"`
}

// AgentConfig controls cmd/agent, the standalone process that actually holds
// the LLM API key and runs the tool-calling loop. Kept out of the bot's own
// container entirely -- the bot only ever talks to it over HTTP via
// LLM.AgentURL/AgentSecret above.
type AgentConfig struct {
	// Address defaults to ":8083" when empty. Container-internal only; it
	// must never be published to the host, same as Internal.Address.
	Address string `toml:"address"`
	// Secret is presented by the bot in the X-Internal-Token header. The
	// server refuses to start when it is empty rather than running an
	// unauthenticated endpoint that spends real API money per request.
	Secret string `toml:"secret"`
	// BaseURL is the OpenAI-compatible chat completions endpoint, e.g.
	// "https://cliproxy.milind.dev/v1/".
	BaseURL string `toml:"base_url"`
	// APIKey is normally left EMPTY here and supplied through
	// STMPD_AGENT_API_KEY instead -- see ResolvedAPIKey. Setting it in the
	// TOML still works, but leaks the key into the bot and dashboard
	// containers, which mount this same file.
	APIKey string `toml:"api_key"`
	Model  string `toml:"model"`
	// MaxTokens caps the length of every completion, including the ones
	// spent on tool-calling round-trips.
	MaxTokens int `toml:"max_tokens"`
}

// AgentAPIKeyEnv holds the LLM API key when it is kept out of the TOML.
const AgentAPIKeyEnv = "STMPD_AGENT_API_KEY"

// ResolvedAPIKey returns the LLM API key, preferring the environment over
// the config file.
//
// The whole point of splitting cmd/agent out of the bot was that it is the
// only process that ever holds this key -- but docker-compose mounts one
// config.docker.toml into the bot, the agent AND the dashboard, so a key
// written in [agent].api_key sits on all three containers' filesystems and
// the isolation is nominal. It can't simply move to its own file: the same
// sharing is what makes the database credentials and the [llm]/[agent]
// shared secret exist exactly once.
//
// So the key alone leaves the file. Compose sets STMPD_AGENT_API_KEY on the
// agent service only, and it exists nowhere else on the box but the .env it
// is read from. The TOML field stays supported for local runs, where one
// process reads one file and there is nothing to leak into.
func (a AgentConfig) ResolvedAPIKey() string {
	if key := os.Getenv(AgentAPIKeyEnv); key != "" {
		return key
	}
	return a.APIKey
}

type StorageConfig struct {
	// BackgroundsDir is where rank card background images live. Defaults to
	// "assets/backgrounds" (the images baked into the bot's own image) when
	// empty, which is what makes local/dev runs work without configuring
	// this at all. In production this points at a volume shared between the
	// bot and dashboard containers, so an upload through the dashboard is
	// immediately usable by the bot without a redeploy.
	BackgroundsDir string `toml:"backgrounds_dir"`
}

type BotConfig struct {
	DevGuilds []snowflake.ID `toml:"dev_guilds"`
	Token     string         `toml:"token"`
	// The tag has to match what the config files actually say. It read
	// "youtube_api_key" while config.toml, config.example.toml and the deployed
	// config.docker.toml all write "yt_api_key", so this field was empty in
	// production and the YouTube service ran on the service-account file alone.
	YoutubeAPIKey      string   `toml:"yt_api_key"`
	GoogleServiceFile  string   `toml:"google_service_file"`
	RedditClientID     string   `toml:"reddit_client_id"`
	RedditClientSecret string   `toml:"reddit_client_secret"`
	RedditBotUsername  string   `toml:"reddit_bot_username"`
	RedditBotPassword  string   `toml:"reddit_bot_password"`
	BeatportUsername   string   `toml:"beatport_username"`
	BeatportPassword   string   `toml:"beatport_password"`
	BeatportLabelID    string   `toml:"beatport_label_id"`
	BeatportArtistIDs  []string `toml:"beatport_artist_ids"`
	BeatportMaxTracks  int      `toml:"beatport_max_tracks"`
}

type LogConfig struct {
	Level      slog.Level `toml:"level"`
	Format     string     `toml:"format"`
	AddSource  bool       `toml:"add_source"`
	File       string     `toml:"file"`
	MaxSize    int        `toml:"max_size"`
	MaxAge     int        `toml:"max_age"`
	MaxBackups int        `toml:"max_backups"`
	TimeZone   string     `toml:"timezone"`
}

type LavalinkConfig struct {
	URL      string `toml:"url"`
	Password string `toml:"password"`
}

// HealthConfig controls the /health endpoint used by the container HEALTHCHECK
// and by external uptime monitoring. Address is a net/http listen address;
// it defaults to ":8081" when left empty.
type HealthConfig struct {
	Address string `toml:"address"`
}

type DatabaseConfig struct {
	Host     string `toml:"host"`
	User     string `toml:"user"`
	Password string `toml:"password"`
	Name     string `toml:"name"`
	Port     int    `toml:"port"`
}

func (d *DatabaseConfig) URI() string {
	uri := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", d.User, d.Password, d.Host, d.Port, d.Name)
	return uri
}

// InternalConfig controls the bot's internal API, which serves live guild data
// (roles, channels, member counts) to the dashboard from the disgo cache so the
// bot token never has to leave this process.
//
// This is a separate listener from the health server on purpose: /health is
// deliberately unauthenticated and is hit by the container HEALTHCHECK, and
// mixing authenticated routes onto that port makes it easy to expose them by
// accident the day someone publishes 8081 to the host.
type InternalConfig struct {
	// Address defaults to ":8082" when empty. Container-internal only; it
	// must never be published to the host.
	Address string `toml:"address"`
	// Secret is presented by the dashboard in the X-Internal-Token header.
	// The API refuses to start when it is empty rather than serving guild
	// data to anything that can reach the port.
	Secret string `toml:"secret"`
}

// DashboardConfig is read only by the dashboard binary (cmd/dashboard).
type DashboardConfig struct {
	// Address defaults to ":8080" when empty.
	Address string `toml:"address"`
	// PublicBaseURL is the externally reachable origin, used to decide
	// whether session cookies get the Secure flag.
	PublicBaseURL string `toml:"public_base_url"`

	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
	// RedirectURI has to match a redirect registered on the Discord
	// application byte for byte or the OAuth exchange fails.
	RedirectURI string `toml:"redirect_uri"`

	// SessionSecret keys the HMAC over the session cookie. At least 32 bytes.
	SessionSecret string `toml:"session_secret"`

	// BotAPIURL points at the bot's InternalConfig.Address, and BotAPISecret
	// must equal InternalConfig.Secret.
	BotAPIURL    string `toml:"bot_api_url"`
	BotAPISecret string `toml:"bot_api_secret"`

	// SuperAdminIDs may administer every guild the bot is in, regardless of
	// their Discord permissions there. Distinct from a Discord guild's own
	// owner -- this is an app-level allowlist of the bot operator's IDs.
	SuperAdminIDs []snowflake.ID `toml:"super_admin_ids"`
}
