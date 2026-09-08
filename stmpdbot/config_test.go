package stmpdbot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestDatabaseConfig_URI(t *testing.T) {
	t.Parallel()

	cfg := DatabaseConfig{
		Host:     "localhost",
		User:     "postgres",
		Password: "password",
		Name:     "garrixbot",
		Port:     5432,
	}

	const want = "postgres://postgres:password@localhost:5432/garrixbot?sslmode=disable"
	if got := cfg.URI(); got != want {
		t.Errorf("URI() = %q, want %q", got, want)
	}
}

// BUG: the URI is assembled with Sprintf and nothing is percent-encoded, so a
// password containing '@', ':' or '/' produces a URI that parses wrongly or not
// at all. Recorded rather than fixed: the deployment's password is alphanumeric,
// and changing this needs a check that no existing config breaks.
func TestDatabaseConfig_URI_DoesNotEscapeThePassword(t *testing.T) {
	t.Parallel()

	cfg := DatabaseConfig{
		Host: "localhost", User: "postgres", Password: "p@ss:w/rd",
		Name: "garrixbot", Port: 5432,
	}

	if got := cfg.URI(); !strings.Contains(got, "p@ss:w/rd") {
		t.Errorf("URI() = %q; the password is now escaped, so this test can go", got)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Parallel()

	t.Run("reads a config file", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(`
[log]
level = "info"
format = "text"

[bot]
token = "abc123"
yt_api_key = "yt-key"

[database]
host = "db.example.com"
user = "postgres"
password = "secret"
name = "garrixbot"
port = 5433

[health]
address = ":9090"
`), 0o600); err != nil {
			t.Fatalf("failed to write the config: %v", err)
		}

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig returned an error: %v", err)
		}

		if cfg.Bot.Token != "abc123" {
			t.Errorf("Bot.Token = %q, want %q", cfg.Bot.Token, "abc123")
		}
		if cfg.Bot.YoutubeAPIKey != "yt-key" {
			t.Errorf("Bot.YoutubeAPIKey = %q, want %q", cfg.Bot.YoutubeAPIKey, "yt-key")
		}
		if cfg.DB.Port != 5433 {
			t.Errorf("DB.Port = %d, want 5433", cfg.DB.Port)
		}
		if cfg.Health.Address != ":9090" {
			t.Errorf("Health.Address = %q, want %q", cfg.Health.Address, ":9090")
		}
	})

	t.Run("a missing file is reported", func(t *testing.T) {
		t.Parallel()

		_, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.toml"))
		if err == nil {
			t.Fatal("expected an error for a missing config file")
		}
		if !strings.Contains(err.Error(), "failed to open config") {
			t.Errorf("error = %q, want it to mention opening the config", err)
		}
	})

	t.Run("malformed TOML is reported", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("[bot\ntoken = "), 0o600); err != nil {
			t.Fatalf("failed to write the config: %v", err)
		}

		if _, err := LoadConfig(path); err == nil {
			t.Fatal("expected an error for malformed TOML")
		}
	})
}

// The example config is the only documentation of what the bot expects, and a
// key that does not match its struct tag decodes silently to a zero value.
// That is exactly what happened: the struct tag said youtube_api_key while the
// example, config.toml and the deployed config.docker.toml all wrote yt_api_key,
// so the bot ran with no YouTube key at all. The deployed file is the authority,
// so the tag moved to yt_api_key and the example follows it.
// Decoding with DisallowUnknownFields catches that and any future drift.
func TestConfigExample_MatchesTheConfigStruct(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("../config.example.toml")
	if err != nil {
		t.Fatalf("failed to read config.example.toml: %v", err)
	}

	var cfg Config
	decoder := toml.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&cfg); err != nil {
		t.Fatalf("config.example.toml does not match the Config struct, so anyone "+
			"copying it gets a silently empty setting: %v", err)
	}
}

// The agent is meant to be the only container holding the LLM API key, but
// docker-compose mounts one config.docker.toml into the bot, the agent and
// the dashboard alike -- so the key has to reach the agent by a route the
// other two don't share. ResolvedAPIKey is that route.
func TestAgentConfig_ResolvedAPIKey(t *testing.T) {
	// No t.Parallel(): t.Setenv and parallel subtests don't mix.

	t.Run("prefers the environment over the config file", func(t *testing.T) {
		t.Setenv(AgentAPIKeyEnv, "from-env")

		cfg := AgentConfig{APIKey: "from-toml"}
		if got := cfg.ResolvedAPIKey(); got != "from-env" {
			t.Errorf("ResolvedAPIKey() = %q, want %q", got, "from-env")
		}
	})

	t.Run("falls back to the config file", func(t *testing.T) {
		t.Setenv(AgentAPIKeyEnv, "")

		cfg := AgentConfig{APIKey: "from-toml"}
		if got := cfg.ResolvedAPIKey(); got != "from-toml" {
			t.Errorf("ResolvedAPIKey() = %q, want %q", got, "from-toml")
		}
	})

	t.Run("is empty when neither is set", func(t *testing.T) {
		t.Setenv(AgentAPIKeyEnv, "")

		if got := (AgentConfig{}).ResolvedAPIKey(); got != "" {
			t.Errorf("ResolvedAPIKey() = %q, want empty", got)
		}
	})
}

// The pre-split [llm] section kept base_url/api_key/model after those fields
// moved to [agent], and go-toml dropped them without a word -- so a stale copy
// of the LLM API key lived in a section the bot reads. Unknown keys still must
// not stop the config loading; they just have to be audible.
func TestLoadConfig_IgnoresUnknownKeysWithoutFailing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[bot]
token = "abc123"

[llm]
enabled = true
agent_url = "http://agent:8083"
api_key = "stale-copy-of-the-key"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() returned %v; an unknown key must not stop the config loading", err)
	}
	if !cfg.LLM.Enabled || cfg.LLM.AgentURL != "http://agent:8083" {
		t.Errorf("known keys were not decoded: %+v", cfg.LLM)
	}
}
