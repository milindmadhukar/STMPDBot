// Command agent is the standalone AI persona service: an HTTP endpoint the
// main bot forwards a triggered mention or reply to.
//
// It is a separate binary from the bot for one reason: every prompt, tool or
// memory change used to mean rebuilding, pushing and restarting the whole
// bot container -- dropping the radio connection and interrupting every
// other feature just to ship an AI tweak. This process can redeploy on its
// own. It reads the same TOML config as the bot (same as cmd/dashboard) so
// the database credentials are defined exactly once, but it is the only
// process that ever holds the LLM API key -- the bot's own container never
// does. That last part is only true because the key comes from
// STMPD_AGENT_API_KEY, set on this service alone: the config file itself is
// mounted into all three containers, so a key written into [agent].api_key
// would sit on the bot's and the dashboard's filesystems too. See
// stmpdbot.AgentConfig.ResolvedAPIKey.
//
// It deliberately does NOT run migrations, for the same reason
// cmd/dashboard doesn't: the bot owns the schema, and two processes racing
// golang-migrate on independent redeploys has no ordering guarantee about
// which wins.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/stmpdbot/ai"
)

func main() {
	configPath := flag.String("config", "config.toml", "path to config")
	flag.Parse()

	cfg, err := stmpdbot.LoadConfig(*configPath)
	if err != nil {
		slog.Error("Failed to read config", slog.Any("err", err))
		os.Exit(1)
	}

	// Shares the bot's logger setup, same as cmd/dashboard.
	stmpdbot.SetupLogger(cfg.Log)

	if cfg.Agent.Secret == "" {
		slog.Error("agent.secret must be set -- refusing to run an unauthenticated endpoint that spends real API money")
		os.Exit(1)
	}
	apiKey := cfg.Agent.ResolvedAPIKey()
	if cfg.Agent.BaseURL == "" || apiKey == "" {
		slog.Error("agent.base_url must be set, and the API key via " + stmpdbot.AgentAPIKeyEnv + " (preferred) or agent.api_key")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := newPool(ctx, cfg.DB.URI())
	if err != nil {
		slog.Error("Failed to connect to database", slog.Any("err", err))
		os.Exit(1)
	}
	defer pool.Close()

	maxTokens := cfg.Agent.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	// Memory is a shared, self-hosted mem0 instance -- shared with the other
	// agents on the same box, segregated from them by agent_id rather than by
	// deployment. NewMemory returns nil when it is unconfigured, and every
	// call site tolerates that.
	memory := ai.NewMemory(cfg.Agent.MemoryURL, cfg.Agent.ResolvedMemoryAPIKey(), cfg.Agent.ResolvedMemoryAgentID())
	if memory.Enabled() {
		slog.Info("Long-term memory enabled",
			slog.String("url", cfg.Agent.MemoryURL),
			slog.String("agent_id", cfg.Agent.ResolvedMemoryAgentID()))
	} else {
		slog.Warn("Long-term memory not configured (agent.memory_url is empty); the persona will not remember anything")
	}

	srv := &server{
		queries: db.New(pool),
		client:  ai.NewClient(cfg.Agent.BaseURL, apiKey, cfg.Agent.Model, maxTokens),
		memory:  memory,
		secret:  cfg.Agent.Secret,
	}

	addr := cfg.Agent.Address
	if addr == "" {
		addr = ":8083"
	}

	slog.Info("Agent service starting", slog.String("addr", addr), slog.String("model", cfg.Agent.Model))
	if err := srv.ListenAndServe(ctx, addr); err != nil {
		slog.Error("Agent service stopped", slog.Any("err", err))
		os.Exit(1)
	}
	slog.Info("Agent service stopped")
}

// newPool mirrors cmd/dashboard's retry loop: the database container and
// this service start together under compose, so the first few connections
// routinely lose the race.
func newPool(ctx context.Context, uri string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, uri)
	if err != nil {
		return nil, err
	}

	const attempts = 5
	for i := range attempts {
		if err = pool.Ping(ctx); err == nil {
			return pool, nil
		}
		if ctx.Err() != nil {
			pool.Close()
			return nil, ctx.Err()
		}
		slog.Warn("Database not ready, retrying", slog.Int("attempt", i+1), slog.Any("err", err))
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}

	pool.Close()
	return nil, err
}
