// Command promote-shared-memories finds the facts hiding inside the persona's
// personal memories and copies them into the shared pool.
//
// Every memory written before 2026-09-08 was scoped "personal", because the
// remember tool described "shared" as being for facts about the server and the
// model read that as facts about the community rather than facts about music.
// So 11,243 memories, including a great deal of what this server collectively
// knows about Martin Garrix, sat visible only to whichever member happened to
// say it. Ask the bot what colour Breakaway's lasers are and it would know only
// if you were the person who told it.
//
// The split is not something a regex can do. "Third Party was announced for
// Don't Let Daddy Know" and "User's phone screen recordings are low resolution"
// are both stated without a "User thinks" prefix, and only one of them is a
// fact about the world. So each memory is judged by a model, in batches.
//
// What gets promoted is deliberately narrow: a claim about music, artists,
// releases, shows, production or this server that stays true with the person
// removed. Anything that needs "User" to make sense is an opinion or a
// biographical detail and stays exactly where it is -- promoting one of those
// would take something a member told the bot in a conversation and hand it to
// the whole server, which is the opposite of what the scope split is for.
//
// Nothing is deleted. A promoted fact is copied, so the member keeps their own
// memory of having said it and the server gains the fact.
//
// Always run -dry-run first.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/milindmadhukar/STMPDBot/scripts/internal/script"
	"github.com/milindmadhukar/STMPDBot/stmpdbot/ai"
)

var (
	guildFlag   = flag.Int64("guild", 690950056202731521, "guild whose shared pool receives the facts")
	classifyN   = flag.Int("classify-batch", 25, "memories per classification call")
	writeN      = flag.Int("write-batch", 15, "facts per mem0 write call")
	concurrency = flag.Int("concurrency", 4, "concurrent model calls")
	maxFacts    = flag.Int("max-facts", 0, "stop after promoting this many facts (0 = no limit)")
	callTimeout = flag.Duration("call-timeout", 4*time.Minute, "timeout for one mem0 write")
	limit       = flag.Int("limit", 0, "examine at most this many memories (0 = all); for validating the prompt cheaply before paying for the whole store")
)

const classifyPrompt = `You are sorting a music Discord server's memories into two piles.

For each numbered memory, decide whether it states a FACT ABOUT THE WORLD that stays
true with the person removed -- about music, artists, tracks, releases, live shows,
production, equipment, or how this server works.

Promote it ONLY if all of these hold:
- it would still be worth knowing if a completely different member had said it
- it is a claim about something outside the speaker, not about the speaker
- it does not need the word "User" to make sense

Do NOT promote:
- opinions, preferences, favourites, excitement, disappointment ("User loves X",
  "User thinks Y is the best") -- these belong to the person who holds them
- anything biographical: where they went, what they own, what they do, their history
- anything about the speaker's own equipment, phone, plans or feelings
- speculation, predictions, or things framed as what somebody hopes or expects
- somebody having shared, posted, linked or submitted something. That is an event in
  a channel, not knowledge about music. "A remake was shared at <url>" is worthless a
  month later; "X's remix of Y was made by Z" is not
- anything whose substance is a URL, or that only makes sense with the link attached
- one-off moments in this Discord: bot commands, image generations, message reactions
- anything about a named individual member of this server

Prefer durable knowledge -- things that will still be true and still be worth knowing
next year. Who made a track, what an ID turned out to be, what was played where, when
something released, what a show's production looked like.

When you promote one, rewrite it as a standalone fact with the person removed, in one
sentence, keeping every specific detail (names, dates, colours, numbers). If a memory
mixes an opinion with a fact, promote only the fact.

Reply with ONLY a JSON array, one object per promoted memory, nothing else:
[{"n": <the memory's number>, "fact": "<the rewritten standalone fact>"}]

If none qualify, reply with exactly: []`

type memory struct {
	id     string
	userID string
	text   string
}

func main() {
	env, ctx, release := script.Setup("promote-shared-memories")
	defer release()

	mem := ai.NewMemory(
		env.Config.Agent.MemoryURL,
		env.Config.Agent.ResolvedMemoryAPIKey(),
		env.Config.Agent.ResolvedMemoryAgentID(),
	)
	if !mem.Enabled() {
		script.Fatal("agent.memory_url is not set in the given config", nil)
	}
	mem.SetWriteTimeout(*callTimeout)

	apiKey := env.Config.Agent.ResolvedAPIKey()
	if env.Config.Agent.BaseURL == "" || apiKey == "" {
		script.Fatal("agent.base_url and an API key are required -- the classification runs on the same model as the persona", nil)
	}
	llm := ai.NewClient(env.Config.Agent.BaseURL, apiKey, env.Config.Agent.Model, 4096)

	memories := loadAll(ctx, env, mem)
	slog.Info("Loaded personal memories", slog.Int("count", len(memories)))
	if *limit > 0 && len(memories) > *limit {
		memories = memories[:*limit]
		slog.Warn("Examining a sample only", slog.Int("limit", *limit))
	}
	if len(memories) == 0 {
		return
	}

	facts := classify(ctx, llm, memories)
	slog.Info("Classified",
		slog.Int("examined", len(memories)),
		slog.Int("world_facts", len(facts)),
		slog.Int("left_personal", len(memories)-len(facts)))

	unique := dedupe(facts)
	slog.Info("After de-duplication",
		slog.Int("unique_facts", len(unique)),
		slog.Int("duplicates_collapsed", len(facts)-len(unique)))

	if *maxFacts > 0 && len(unique) > *maxFacts {
		unique = unique[:*maxFacts]
		slog.Warn("Capped", slog.Int("promoting", len(unique)))
	}

	if env.DryRun {
		for i, f := range unique {
			if i >= 25 {
				break
			}
			slog.Info("  would promote", slog.String("fact", f))
		}
		slog.Info("Dry run, nothing written",
			slog.Int("would_promote", len(unique)),
			slog.Int("mem0_write_calls", (len(unique)+*writeN-1) / *writeN))
		return
	}

	promote(ctx, mem, unique)
}

// loadAll pages every personal memory out of mem0, one member at a time. The
// bulk listing endpoint caps at 1000 and this store holds an order of
// magnitude more, so per-user listing is the only complete read.
func loadAll(ctx context.Context, env *script.Env, mem *ai.Memory) []memory {
	rows, err := env.Pool.Query(ctx, `
		SELECT DISTINCT author_id FROM messages WHERE author_id IS NOT NULL`)
	if err != nil {
		script.Fatal("failed to list members", err)
	}
	defer rows.Close()

	var users []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			script.Fatal("failed to scan a member", err)
		}
		users = append(users, id)
	}

	var (
		mu  sync.Mutex
		out []memory
		wg  sync.WaitGroup
		sem = make(chan struct{}, 8)
	)
	prog := script.NewProgress("read memories", len(users))
	for _, u := range users {
		wg.Add(1)
		go func(u int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			key := ai.UserKey(u)
			records, err := mem.List(ctx, key, 1000)
			if err != nil {
				slog.Warn("Could not read a member's memories", slog.Int64("user_id", u), slog.Any("err", err))
			}
			mu.Lock()
			for _, r := range records {
				if strings.TrimSpace(r.Memory) != "" {
					out = append(out, memory{id: r.ID, userID: key, text: r.Memory})
				}
			}
			prog.Step()
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	prog.Done()
	return out
}

func classify(ctx context.Context, llm *ai.Client, memories []memory) []string {
	type batch struct {
		start int
		items []memory
	}
	var batches []batch
	for i := 0; i < len(memories); i += *classifyN {
		batches = append(batches, batch{start: i, items: memories[i:min(i+*classifyN, len(memories))]})
	}

	var (
		mu    sync.Mutex
		facts []string
		wg    sync.WaitGroup
		sem   = make(chan struct{}, max(*concurrency, 1))
	)
	prog := script.NewProgress("classify", len(batches))

	for _, b := range batches {
		wg.Add(1)
		go func(b batch) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var sb strings.Builder
			for i, m := range b.items {
				fmt.Fprintf(&sb, "%d. %s\n", i+1, m.text)
			}

			reply, err := llm.ChatCompletion(ctx, []ai.Message{
				{Role: "system", Content: classifyPrompt},
				{Role: "user", Content: sb.String()},
			}, nil)

			mu.Lock()
			defer mu.Unlock()
			prog.Step()
			if err != nil {
				slog.Warn("Classification batch failed", slog.Any("err", err))
				return
			}
			facts = append(facts, parseFacts(reply.Content)...)
		}(b)
	}
	wg.Wait()
	prog.Done()
	return facts
}

// parseFacts is forgiving: models wrap JSON in prose or fences often enough
// that being strict here would throw away most of a paid-for batch.
func parseFacts(content string) []string {
	content = strings.TrimSpace(content)
	if i := strings.Index(content, "["); i >= 0 {
		if j := strings.LastIndex(content, "]"); j > i {
			content = content[i : j+1]
		}
	}

	var rows []struct {
		N    int    `json:"n"`
		Fact string `json:"fact"`
	}
	if err := json.Unmarshal([]byte(content), &rows); err != nil {
		return nil
	}

	var out []string
	for _, r := range rows {
		f := strings.TrimSpace(r.Fact)
		// A rewrite that still says "User" did not actually de-personalise
		// anything, and is exactly the leak this pass must not cause.
		if f == "" || strings.Contains(strings.ToLower(f), "user") {
			continue
		}
		// A "fact" whose substance is a link is a channel event, not
		// knowledge: the URL rots, and what is left says nothing.
		if isMostlyLink(f) {
			continue
		}
		out = append(out, f)
	}
	return out
}

var urlPattern = regexp.MustCompile(`https?://\S+`)

// isMostlyLink reports whether removing the URLs leaves too little to be worth
// remembering.
func isMostlyLink(f string) bool {
	stripped := strings.TrimSpace(urlPattern.ReplaceAllString(f, ""))
	if len(urlPattern.FindAllString(f, -1)) == 0 {
		return false
	}
	// Half the sentence gone with the link, or under a dozen words left, and
	// the link was the point.
	return len(stripped) < len(f)/2 || len(strings.Fields(stripped)) < 12
}

// dedupe collapses the same fact arrived at from different members' memories.
// mem0 would reconcile them on write anyway, but each write costs an
// extraction call, so paying for a hundred phrasings of "Breakaway's lasers
// are green and purple" is money for nothing.
func dedupe(facts []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range facts {
		key := strings.ToLower(strings.Join(strings.Fields(f), " "))
		key = strings.Trim(key, ".!?\"' ")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func promote(ctx context.Context, mem *ai.Memory, facts []string) {
	key := ai.SharedKey(*guildFlag)
	calls := (len(facts) + *writeN - 1) / *writeN
	prog := script.NewProgress("promote", calls)
	defer prog.Done()

	var written, failed int
	for start := 0; start < len(facts); start += *writeN {
		end := min(start+*writeN, len(facts))

		msgs := make([]ai.Mem0Message, 0, end-start)
		for _, f := range facts[start:end] {
			msgs = append(msgs, ai.Mem0Message{Role: "user", Content: f})
		}

		records, err := mem.AddConversation(ctx, key, msgs, map[string]any{
			"scope":    "shared",
			"guild_id": strconv.FormatInt(*guildFlag, 10),
			"source":   "promoted-from-personal",
		})
		if err != nil {
			failed++
			slog.Warn("Promotion batch failed", slog.Any("err", err))
		} else {
			written += len(records)
		}
		prog.Step()

		if ctx.Err() != nil {
			slog.Warn("Stopping early", slog.Any("err", ctx.Err()))
			return
		}
	}
	slog.Info("Promoted", slog.Int("shared_memories_written", written), slog.Int("batches_failed", failed))
}
