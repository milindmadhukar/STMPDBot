// Command analyze-fandom-voice distills a random sample of this server's own
// message history into the static "voice guide" the AI persona feature's
// system prompt is built from (stmpdbot/ai/persona.md, see stmpdbot/ai for
// how it is used).
//
// Read-only: it never writes to the database, only to the output file. The
// only step that costs API money is the LLM synthesis pass -- pass -no-llm
// to see the deterministic stats for free before spending anything.
//
// This is a manual authoring step, not something the running bot ever
// triggers: re-run it by hand whenever the voice guide should be refreshed.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"

	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/scripts/internal/script"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/stmpdbot/ai"
)

var (
	guildID    = flag.Int64("guild", 0, "guild id to sample messages from (required)")
	sampleSize = flag.Int("sample-size", 5000, "how many random messages to sample")
	noLLM      = flag.Bool("no-llm", false, "skip the LLM synthesis step and only print deterministic stats")
	outPath    = flag.String("out", "stmpdbot/ai/persona.md", "where to write the generated voice guide")
)

func main() {
	env, ctx, cleanup := script.Setup("analyze-fandom-voice")
	defer cleanup()

	if *guildID == 0 {
		script.Fatal("-guild is required", nil)
	}

	rows, err := env.Queries.GetRandomMessageSample(ctx, db.GetRandomMessageSampleParams{
		GuildID:  *guildID,
		RowLimit: int32(*sampleSize),
	})
	if err != nil {
		script.Fatal("failed to sample messages", err)
	}
	slog.Info("Sampled messages", slog.Int("count", len(rows)))

	stats := analyze(rows)
	printStats(stats)

	if *noLLM {
		slog.Info("-no-llm set, skipping synthesis; nothing written")
		return
	}

	apiKey := env.Config.Agent.ResolvedAPIKey()
	if !env.Config.LLM.Enabled || apiKey == "" {
		script.Fatal("llm.enabled is off, or no API key in agent.api_key/"+stmpdbot.AgentAPIKeyEnv+" -- pass -no-llm to skip synthesis", nil)
	}

	client := ai.NewClient(env.Config.Agent.BaseURL, apiKey, env.Config.Agent.Model, 1500)
	guide, err := synthesize(ctx, client, rows, stats)
	if err != nil {
		script.Fatal("failed to synthesize the voice guide", err)
	}

	if err := os.WriteFile(*outPath, []byte(guide), 0o644); err != nil {
		script.Fatal("failed to write the voice guide", err)
	}
	slog.Info("Wrote voice guide", slog.String("path", *outPath), slog.Int("bytes", len(guide)))
}

type voiceStats struct {
	messageCount int
	avgLength    float64
	topEmojis    []count
	topWords     []count
	topBigrams   []count
}

type count struct {
	term string
	n    int
}

var emojiPattern = regexp.MustCompile(`[\x{1F300}-\x{1FAFF}\x{2600}-\x{27BF}\x{2190}-\x{21FF}]`)

// stopWords is deliberately short: this feeds a synthesis prompt a human (or
// the model itself) will read critically, not a search index, so it only
// needs to keep the most useless filler out of "top words".
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the a an and or but is are was were be been being
		to of in on at for with as by from this that it its im i you your yeah lol
		just like so not no yes if but do does did have has had can cant will wont`) {
		stopWords[w] = true
	}
}

func analyze(rows []string) voiceStats {
	wordCounts := map[string]int{}
	bigramCounts := map[string]int{}
	emojiCounts := map[string]int{}
	totalLen := 0

	for _, content := range rows {
		totalLen += len(content)

		for _, e := range emojiPattern.FindAllString(content, -1) {
			emojiCounts[e]++
		}

		words := strings.Fields(strings.ToLower(content))
		var kept []string
		for _, w := range words {
			w = strings.Trim(w, ".,!?;:()[]\"'“”‘’")
			if len(w) < 3 || stopWords[w] || strings.HasPrefix(w, "http") {
				continue
			}
			kept = append(kept, w)
			wordCounts[w]++
		}
		for i := 0; i+1 < len(kept); i++ {
			bigramCounts[kept[i]+" "+kept[i+1]]++
		}
	}

	avg := 0.0
	if len(rows) > 0 {
		avg = float64(totalLen) / float64(len(rows))
	}

	return voiceStats{
		messageCount: len(rows),
		avgLength:    avg,
		topEmojis:    top(emojiCounts, 20),
		topWords:     top(wordCounts, 40),
		topBigrams:   top(bigramCounts, 25),
	}
}

func top(counts map[string]int, n int) []count {
	items := make([]count, 0, len(counts))
	for term, c := range counts {
		items = append(items, count{term, c})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].n > items[j].n })
	if len(items) > n {
		items = items[:n]
	}
	return items
}

func printStats(s voiceStats) {
	slog.Info("Deterministic stats",
		slog.Int("messages", s.messageCount),
		slog.Float64("avg_length_chars", s.avgLength))
	fmt.Println("\nTop emojis:", joinCounts(s.topEmojis))
	fmt.Println("\nTop words:", joinCounts(s.topWords))
	fmt.Println("\nTop phrases:", joinCounts(s.topBigrams))
}

func joinCounts(items []count) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = fmt.Sprintf("%s(%d)", it.term, it.n)
	}
	return strings.Join(parts, " ")
}

// synthesize makes one LLM call handing it the deterministic stats plus a
// bounded batch of raw sample messages, and asks for a compact style guide.
// Only message content is sent -- no author, no channel, nothing that
// identifies who said what.
func synthesize(ctx context.Context, client *ai.Client, rows []string, stats voiceStats) (string, error) {
	const maxSampleInPrompt = 400
	sample := rows
	if len(sample) > maxSampleInPrompt {
		sample = sample[:maxSampleInPrompt]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Deterministic stats over %d sampled messages (avg length %.0f chars):\n", stats.messageCount, stats.avgLength)
	fmt.Fprintf(&b, "Top emojis: %s\n", joinCounts(stats.topEmojis))
	fmt.Fprintf(&b, "Top words: %s\n", joinCounts(stats.topWords))
	fmt.Fprintf(&b, "Top phrases: %s\n\n", joinCounts(stats.topBigrams))
	fmt.Fprintf(&b, "Raw sample of %d real messages from the server, one per line, in no particular order:\n", len(sample))
	for _, m := range sample {
		m = strings.ReplaceAll(m, "\n", " ")
		if m == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(m)
		b.WriteString("\n")
	}

	prompt := `You are analyzing a random sample of real chat messages from a Discord server for fans of the DJ/producer Martin Garrix and his label STMPD RCRDS, to write a short style guide for a chatbot that will speak in this community's voice.

Write a compact voice guide covering: overall tone, vocabulary and slang actually used, recurring in-jokes or references you can see repeating, formatting habits (capitalization, emoji use, punctuation), and anything distinctive. Be specific and back claims with what you actually saw in the sample -- do not invent details.

Output plain markdown, no more than 40 lines, ready to drop directly into a system prompt. Do not mention that this came from a data sample.

` + b.String()

	reply, err := client.ChatCompletion(ctx, []ai.Message{
		{Role: "user", Content: prompt},
	}, nil)
	if err != nil {
		return "", err
	}
	if reply.Content == "" {
		return "", fmt.Errorf("synthesize: model returned an empty voice guide")
	}
	return reply.Content, nil
}
