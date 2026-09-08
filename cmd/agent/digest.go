package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/milindmadhukar/STMPDBot/stmpdbot/ai"
)

// The nightly digest is the answer to the only hard question about memory in a
// server this size: how does it stay current when nobody can read every
// message?
//
// You do not read every message. Three things work together:
//
//  1. The conversational path. When somebody actually talks to the bot, the
//     model decides what to keep, inside a round-trip already being paid for.
//     Highest signal, zero marginal cost, and it is where most memory comes
//     from.
//  2. This pass, which sweeps what was said when the bot was NOT addressed --
//     the great majority of a channel -- and folds the small salient fraction
//     in. On this server that is ~200 messages a day, of which the filter
//     keeps roughly 25, which is one to three extraction calls a night.
//  3. mem0's own reconciliation. add() retrieves similar memories and decides
//     ADD / UPDATE / NOOP, so a fact restated months later refreshes in place
//     instead of becoming a second, contradictory copy. Nothing has to audit
//     anything: the store settles it.
//
// The caps below are what make the bill flat. This server's worst day in the
// last fortnight was 1,186 messages, which the filter would reduce to ~140
// candidates -- still under the cap, but the cap is what guarantees a raid or
// an import cannot turn into a four-figure extraction run overnight.

const (
	// digestInterval is how often a pass is due. Daily, deliberately: memory
	// that arrives a day late is indistinguishable from memory that arrived
	// instantly, and a slower cadence gives mem0 more of a conversation to
	// extract from at once.
	digestInterval = 24 * time.Hour

	// digestCheckEvery is how often the loop wakes to ask whether a pass is
	// due. Short relative to the interval so a redeploy cannot push the run
	// a whole day later, and the watermark in postgres keeps it honest.
	digestCheckEvery = time.Hour

	// digestMaxCalls bounds one pass no matter what happened in the server.
	digestMaxCalls = 25

	// digestBatchSize matches what the backfill settled on: big enough that
	// extraction has real dialogue to work with, small enough that a call
	// finishes well inside its timeout.
	digestBatchSize = 20

	// digestMaxPerAuthor stops one very talkative person consuming the whole
	// budget while everyone else in the window is ignored.
	digestMaxPerAuthor = 40

	// digestFirstWindow is how far back the very first pass reaches when
	// there is no watermark yet. Deliberately short: the history backfill
	// (scripts/seed-agent-memory) is what covers everything before this, and
	// duplicating four years of it here would be an expensive accident.
	digestFirstWindow = 48 * time.Hour

	// digestCallTimeout matches the backfill's: a batch of real messages is a
	// far bigger prompt than a single assertion, and the interactive timeout
	// is much too short for it.
	digestCallTimeout = 4 * time.Minute
)

// runDigestLoop runs until ctx is cancelled. It is started only when memory is
// configured.
func runDigestLoop(ctx context.Context, pool *pgxpool.Pool, mem *ai.Memory) {
	// Deliberately not on a fresh boot: a redeploy should not trigger a pass,
	// or a busy day of pushes would run several.
	ticker := time.NewTicker(digestCheckEvery)
	defer ticker.Stop()

	slog.Info("Memory digest scheduled",
		slog.Duration("interval", digestInterval),
		slog.Int("max_calls_per_pass", digestMaxCalls))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := maybeRunDigest(ctx, pool, mem); err != nil {
				slog.Error("Memory digest failed", slog.Any("err", err))
			}
		}
	}
}

func maybeRunDigest(ctx context.Context, pool *pgxpool.Pool, mem *ai.Memory) error {
	lastRun, watermark, err := digestState(ctx, pool)
	if err != nil {
		return err
	}
	if time.Since(lastRun) < digestInterval {
		return nil
	}

	return runDigest(ctx, pool, mem, watermark)
}

// digestState reads the watermark, treating a missing table as "not yet
// migrated" rather than an error. The bot owns the schema and the agent
// deliberately does not migrate, so on a deploy where the agent lands first
// this table simply is not there yet -- which is a reason to wait an hour, not
// to log an error every hour.
func digestState(ctx context.Context, pool *pgxpool.Pool) (lastRun, watermark time.Time, err error) {
	row := pool.QueryRow(ctx, `SELECT last_run_at, watermark FROM agent_memory_digest WHERE id = 1`)
	switch err := row.Scan(&lastRun, &watermark); {
	case err == nil:
		return lastRun, watermark, nil
	case errors.Is(err, pgx.ErrNoRows):
		// First ever run. Reach back a short way only -- the backfill covers
		// history, and this pass exists to keep up, not to catch up.
		return time.Time{}, time.Now().Add(-digestFirstWindow), nil
	default:
		if isUndefinedTable(err) {
			slog.Debug("Memory digest waiting for its table to be migrated")
			return time.Now(), time.Now(), nil
		}
		return time.Time{}, time.Time{}, fmt.Errorf("digest: failed to read state: %w", err)
	}
}

func isUndefinedTable(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "42P01"
}

func runDigest(ctx context.Context, pool *pgxpool.Pool, mem *ai.Memory, since time.Time) error {
	start := time.Now()

	// Raw SQL rather than a sqlc query: this shape belongs to the digest, not
	// to the bot's query surface, the same reason the backfill script uses
	// one. The join is what enforces MinAuthorMessages -- the bot should not
	// be forming memories about somebody who has barely spoken here.
	rows, err := pool.Query(ctx, `
		SELECT m.author_id, m.guild_id, m.content, m.timestamp
		FROM messages m
		JOIN (
			SELECT author_id FROM messages
			WHERE author_id IS NOT NULL
			GROUP BY author_id HAVING count(*) >= $1
		) active ON active.author_id = m.author_id
		WHERE m.author_id IS NOT NULL
		  AND m.timestamp > $2
		ORDER BY m.author_id, m.timestamp DESC`,
		ai.MinAuthorMessages, since)
	if err != nil {
		return fmt.Errorf("digest: failed to read messages: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		guildID  int64
		messages []ai.Mem0Message
	}
	var (
		byAuthor  = map[int64]*candidate{}
		seen      int
		highWater = since
	)

	for rows.Next() {
		var (
			authorID, guildID int64
			content           string
			ts                time.Time
		)
		if err := rows.Scan(&authorID, &guildID, &content, &ts); err != nil {
			return fmt.Errorf("digest: failed to scan a message: %w", err)
		}
		seen++
		if ts.After(highWater) {
			highWater = ts
		}

		kept, verdict := ai.Judge(content)
		if verdict != ai.Keep {
			continue
		}

		c := byAuthor[authorID]
		if c == nil {
			c = &candidate{guildID: guildID}
			byAuthor[authorID] = c
		}
		if len(c.messages) >= digestMaxPerAuthor {
			continue
		}
		c.messages = append(c.messages, ai.Mem0Message{Role: "user", Content: kept})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("digest: failed while reading messages: %w", err)
	}

	// A longer leash than the interactive path, for the same reason the
	// backfill needs one: this sends batches, not sentences.
	mem.SetWriteTimeout(digestCallTimeout)

	var calls, written int
	for authorID, c := range byAuthor {
		for start := 0; start < len(c.messages) && calls < digestMaxCalls; start += digestBatchSize {
			end := min(start+digestBatchSize, len(c.messages))

			records, err := mem.AddConversation(ctx, ai.UserKey(authorID), c.messages[start:end], map[string]any{
				"scope":    "personal",
				"guild_id": fmt.Sprint(c.guildID),
				"source":   "nightly-digest",
			})
			calls++
			if err != nil {
				slog.Warn("digest: batch failed", slog.Int64("author_id", authorID), slog.Any("err", err))
				continue
			}
			written += len(records)
		}
		if calls >= digestMaxCalls {
			slog.Warn("digest: hit the per-pass call cap, the rest waits for tomorrow",
				slog.Int("cap", digestMaxCalls))
			break
		}
	}

	// The watermark advances even when nothing was written. The window was
	// read; re-reading it tomorrow would pay for the same messages twice.
	if err := saveDigestState(ctx, pool, highWater, seen, written); err != nil {
		return err
	}

	slog.Info("Memory digest complete",
		slog.Int("messages_seen", seen),
		slog.Int("authors", len(byAuthor)),
		slog.Int("mem0_calls", calls),
		slog.Int("memories_written", written),
		slog.Duration("took", time.Since(start).Truncate(time.Second)))
	return nil
}

func saveDigestState(ctx context.Context, pool *pgxpool.Pool, watermark time.Time, seen, written int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_digest (id, last_run_at, watermark, messages_seen, memories_written)
		VALUES (1, now(), $1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET
			last_run_at      = now(),
			watermark        = EXCLUDED.watermark,
			messages_seen    = EXCLUDED.messages_seen,
			memories_written = EXCLUDED.memories_written`,
		watermark, seen, written)
	if err != nil {
		return fmt.Errorf("digest: failed to save state: %w", err)
	}
	return nil
}
