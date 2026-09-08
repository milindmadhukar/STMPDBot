// Command seed-agent-memory mines the bot's stored Discord history for things
// worth remembering and writes them into the shared mem0 instance, so the
// persona starts out already knowing the people it has been sitting in a
// channel with for four years.
//
// The corpus is half a million messages and every mem0 write costs a
// fact-extraction LLM call, so feeding it the lot is neither affordable nor
// useful -- most of those messages are "lol". The value is concentrated in a
// small fraction of them, and the filter below is what finds it:
//
//   - the author has to be a real member of the community (minMessages), not
//     a drive-by who posted twice in 2023
//   - the message has to be substantial (minLength). On this corpus that is
//     ~12% of everything, and it is where self-disclosure actually lives
//   - commands, bare links and pure-emoji reactions are dropped outright
//   - anything matching a sensitive pattern is dropped and never sent
//
// What survives is grouped per author and handed to mem0 in batches, because
// its extraction is far better at pulling durable facts out of a run of real
// dialogue than out of one sentence at a time -- and one call per batch is
// what keeps the bill in the tens of dollars rather than the thousands.
//
// Always run -dry-run first. It reports exactly what would be sent and what it
// would cost, and writes nothing.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/milindmadhukar/STMPDBot/scripts/internal/script"
	"github.com/milindmadhukar/STMPDBot/stmpdbot/ai"
)

var (
	minMessages  = flag.Int("min-messages", ai.MinAuthorMessages, "skip authors with fewer messages than this in the whole corpus")
	perAuthor    = flag.Int("per-author", 120, "at most this many messages per author, newest first")
	batchSize    = flag.Int("batch", 20, "messages per mem0 extraction call")
	maxAuthors   = flag.Int("max-authors", 0, "stop after this many authors (0 = no limit)")
	sinceDays    = flag.Int("since-days", 0, "only consider messages newer than this many days (0 = all history)")
	guildFlag    = flag.Int64("guild", 0, "only this guild (0 = all)")
	includeLegcy = flag.Bool("include-legacy", false, "also migrate rows from the old agent_memory table")
	concurrency  = flag.Int("concurrency", 3, "how many mem0 extraction calls to run at once")
	skipSeeded   = flag.Bool("skip-seeded", true, "skip authors this pass has already seeded (see resume)")
	callTimeout  = flag.Duration("call-timeout", 4*time.Minute, "how long to wait for one mem0 extraction call")
)

// sensitive drops a message outright rather than trusting the extraction model
// to leave it alone. These are things nobody consented to having stored in a
// vector database, and the cheapest place to enforce that is before the API
// call, not after.
type authorBatch struct {
	authorID int64
	guildID  int64
	messages []string
}

func main() {
	env, ctx, release := script.Setup("seed-agent-memory")
	defer release()

	mem := ai.NewMemory(
		env.Config.Agent.MemoryURL,
		env.Config.Agent.ResolvedMemoryAPIKey(),
		env.Config.Agent.ResolvedMemoryAgentID(),
	)
	if !mem.Enabled() {
		script.Fatal("agent.memory_url is not set in the given config -- there is nowhere to write to", nil)
	}
	// A batch of real messages is a far bigger prompt than the single
	// assertion the interactive path sends, and extraction scales with it.
	mem.SetWriteTimeout(*callTimeout)

	slog.Info("Target",
		slog.String("mem0", env.Config.Agent.MemoryURL),
		slog.String("agent_id", env.Config.Agent.ResolvedMemoryAgentID()),
		slog.Bool("dry_run", env.DryRun))

	if *includeLegcy {
		migrateLegacy(ctx, env, mem)
	}

	batches, stats := collect(ctx, env)

	if *skipSeeded {
		batches = dropSeeded(ctx, mem, batches)
	}
	slog.Info("Filtered the corpus",
		slog.Int("messages_total", stats.total),
		slog.Int("passed_filter", stats.kept),
		slog.Int("dropped_short", stats.short),
		slog.Int("dropped_noise", stats.noise),
		slog.Int("dropped_sensitive", stats.sensitive),
		slog.Int("authors", len(batches)))

	calls := 0
	for _, b := range batches {
		calls += (len(b.messages) + *batchSize - 1) / *batchSize
	}
	slog.Info("Planned work",
		slog.Int("mem0_calls", calls),
		slog.Int("concurrency", max(*concurrency, 1)),
		// ~35s per batched call, measured on this corpus. A single-sentence
		// write is ~8s; a batch of dozens of messages is not.
		slog.String("est_wall_clock",
			(time.Duration(calls)*35*time.Second/time.Duration(max(*concurrency, 1))).Truncate(time.Minute).String()))

	if env.DryRun {
		sample(batches)
		slog.Info("Dry run, nothing written")
		return
	}

	send(ctx, mem, batches, calls)
}

type stats struct{ total, kept, short, noise, sensitive int }

// collect walks the message history and returns the surviving messages grouped
// by author, newest first.
func collect(ctx context.Context, env *script.Env) ([]authorBatch, stats) {
	var st stats

	// Straight SQL rather than a sqlc query: this shape is unique to the
	// script, and generating a query for a one-off pass would put it in the
	// bot's own query surface forever.
	rows, err := env.Pool.Query(ctx, `
		SELECT m.author_id, m.guild_id, m.content, m.timestamp
		FROM messages m
		JOIN (
			SELECT author_id FROM messages
			WHERE author_id IS NOT NULL
			GROUP BY author_id HAVING count(*) >= $1
		) active ON active.author_id = m.author_id
		WHERE m.author_id IS NOT NULL
		  AND ($2 = 0 OR m.guild_id = $2)
		  AND ($3 = 0 OR m.timestamp > now() - make_interval(days => $3))
		ORDER BY m.author_id, m.timestamp DESC`,
		*minMessages, *guildFlag, *sinceDays)
	if err != nil {
		script.Fatal("failed to read the message history", err)
	}
	defer rows.Close()

	byAuthor := map[int64]*authorBatch{}
	for rows.Next() {
		var (
			authorID, guildID int64
			content           string
			ts                time.Time
		)
		if err := rows.Scan(&authorID, &guildID, &content, &ts); err != nil {
			script.Fatal("failed to scan a message", err)
		}
		st.total++

		// Shared with the nightly digest -- see stmpdbot/ai/salience.go for
		// why the two passes must not drift apart.
		kept, verdict := ai.Judge(content)
		switch verdict {
		case ai.TooShort:
			st.short++
			continue
		case ai.Noise:
			st.noise++
			continue
		case ai.Sensitive:
			st.sensitive++
			continue
		}
		content = kept

		b := byAuthor[authorID]
		if b == nil {
			b = &authorBatch{authorID: authorID, guildID: guildID}
			byAuthor[authorID] = b
		}
		// Rows arrive newest-first, so the cap keeps the most recent -- who
		// somebody is now matters more than who they were in 2022.
		if len(b.messages) >= *perAuthor {
			continue
		}
		b.messages = append(b.messages, content)
		st.kept++
	}
	if err := rows.Err(); err != nil {
		script.Fatal("failed while reading the message history", err)
	}

	out := make([]authorBatch, 0, len(byAuthor))
	for _, b := range byAuthor {
		out = append(out, *b)
	}
	// Most-active authors first: if a run is interrupted or capped, the people
	// the bot talks to most are the ones already done.
	sort.Slice(out, func(i, j int) bool { return len(out[i].messages) > len(out[j].messages) })
	if *maxAuthors > 0 && len(out) > *maxAuthors {
		out = out[:*maxAuthors]
	}
	return out, st
}

type job struct {
	key      string
	guildID  int64
	authorID int64
	messages []ai.Mem0Message
}

// send runs the batches concurrently. Each one costs mem0 an extraction call
// of about eight seconds, so serially this pass takes an hour and a half for
// no reason -- the work is entirely latency, not load. Concurrency is modest
// and configurable because the far side is one self-hosted container sharing a
// box with everything else.
//
// Batches for the same author are independent: mem0 reconciles overlapping
// facts on its own, which is the property this whole design leans on.
// dropSeeded removes authors this pass has already covered.
//
// It exists because the pass takes hours and gets interrupted -- a quota
// running out, a redeploy, a laptop closing -- and without this, resuming
// means paying for every extraction again from the top. mem0 would reconcile
// the duplicates, so nothing would break; it would just be an hour of
// somebody's money spent to learn what was already known.
//
// The record of what is done lives in mem0 itself rather than in a checkpoint
// file, because mem0 is the thing that actually has the memories. A file can
// disagree with reality; the store cannot. Each check is a cheap listing with
// no extraction behind it, so this costs latency and no tokens at all.
func dropSeeded(ctx context.Context, mem *ai.Memory, batches []authorBatch) []authorBatch {
	var (
		mu      sync.Mutex
		keep    = make([]authorBatch, 0, len(batches))
		skipped int
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 8)
	)

	for _, b := range batches {
		wg.Add(1)
		go func(b authorBatch) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			records, err := mem.List(ctx, ai.UserKey(b.authorID), 100)
			if err != nil {
				// Err towards doing the work: a listing that failed is not
				// evidence that the author was seeded.
				slog.Debug("Could not check whether an author was seeded",
					slog.Int64("author_id", b.authorID), slog.Any("err", err))
			}

			for _, r := range records {
				if r.Metadata["source"] == "history-backfill" {
					mu.Lock()
					skipped++
					mu.Unlock()
					return
				}
			}

			mu.Lock()
			keep = append(keep, b)
			mu.Unlock()
		}(b)
	}
	wg.Wait()

	// Restore the most-active-first order the concurrent checks scrambled, so
	// an interrupted resume still covers the busiest people first.
	sort.Slice(keep, func(i, j int) bool { return len(keep[i].messages) > len(keep[j].messages) })

	slog.Info("Resume check",
		slog.Int("already_seeded", skipped),
		slog.Int("still_to_do", len(keep)))
	return keep
}

func send(ctx context.Context, mem *ai.Memory, batches []authorBatch, calls int) {
	progress := script.NewProgress("seed-agent-memory", calls)
	defer progress.Done()

	jobs := make(chan job)
	go func() {
		defer close(jobs)
		for _, b := range batches {
			for start := 0; start < len(b.messages); start += *batchSize {
				end := min(start+*batchSize, len(b.messages))

				// Every message goes in as the user's own turn. mem0's
				// extraction decides which of them hold a durable fact --
				// this script only stops obvious waste from reaching it.
				msgs := make([]ai.Mem0Message, 0, end-start)
				for _, m := range b.messages[start:end] {
					msgs = append(msgs, ai.Mem0Message{Role: "user", Content: m})
				}
				select {
				case jobs <- job{key: ai.UserKey(b.authorID), guildID: b.guildID, authorID: b.authorID, messages: msgs}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	var (
		mu              sync.Mutex
		written, failed int
		wg              sync.WaitGroup
	)
	workers := max(*concurrency, 1)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				records, err := mem.AddConversation(ctx, j.key, j.messages, map[string]any{
					"scope":    "personal",
					"guild_id": fmt.Sprint(j.guildID),
					"source":   "history-backfill",
				})

				mu.Lock()
				if err != nil {
					failed++
					slog.Warn("Batch failed", slog.Int64("author_id", j.authorID), slog.Any("err", err))
				} else {
					written += len(records)
				}
				progress.Step()
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	slog.Info("Seeded",
		slog.Int("memories_written", written),
		slog.Int("batches_failed", failed))
}

// migrateLegacy carries the handful of rows from the old postgres-backed
// memory over. They go in as ordinary writes so mem0 reconciles them the same
// way as anything else -- two of the three existing rows contradict each
// other, and letting the extraction settle that is the whole point.
func migrateLegacy(ctx context.Context, env *script.Env, mem *ai.Memory) {
	rows, err := env.Pool.Query(ctx, `SELECT scope, scope_id, guild_id, content FROM agent_memory ORDER BY created_at`)
	if err != nil {
		script.Fatal("failed to read agent_memory", err)
	}
	defer rows.Close()

	var migrated int
	for rows.Next() {
		var (
			scope            string
			scopeID, guildID int64
			content          string
		)
		if err := rows.Scan(&scope, &scopeID, &guildID, &content); err != nil {
			script.Fatal("failed to scan an agent_memory row", err)
		}

		key := ai.UserKey(scopeID)
		newScope := "personal"
		if scope == "guild" {
			key = ai.SharedKey(guildID)
			newScope = "shared"
		}

		if env.DryRun {
			slog.Info("Would migrate", slog.String("scope", newScope), slog.String("content", content))
			migrated++
			continue
		}
		if _, err := mem.Add(ctx, key, content, map[string]any{
			"scope": newScope, "guild_id": fmt.Sprint(guildID), "source": "legacy-agent-memory",
		}); err != nil {
			slog.Warn("Failed to migrate a row", slog.Any("err", err))
			continue
		}
		migrated++
	}
	slog.Info("Legacy memories", slog.Int("migrated", migrated), slog.Bool("dry_run", env.DryRun))
}

// sample prints what a dry run would actually send, because a count is not
// enough to judge whether the filter is picking up the right things.
func sample(batches []authorBatch) {
	shown := 0
	for _, b := range batches {
		if shown >= 3 {
			return
		}
		slog.Info("Sample author",
			slog.Int64("author_id", b.authorID), slog.Int("messages", len(b.messages)))
		for i, m := range b.messages {
			if i >= 3 {
				break
			}
			if len(m) > 160 {
				m = m[:160] + "..."
			}
			slog.Info("  would send", slog.String("message", m))
		}
		shown++
	}
}
