package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Memory lives in a shared self-hosted mem0 instance (see mem0.go), not in
// this bot's own database. That move is what turned a per-(guild, user) key
// -value table into something that behaves like memory: facts are found by
// meaning rather than exact scope, a person is the same person in every
// server the bot is in, and re-asserting something months later updates it in
// place instead of piling up a second copy.
//
// Two shapes of memory, and the split is a privacy boundary as much as a
// design one:
//
//   - Personal, keyed by UserKey. Recalled only in a conversation with that
//     person. What they like, what they do, the running joke.
//   - Shared, keyed by SharedKey(guild). Recalled by anyone in that server.
//     What the server is for, who runs it, community-wide in-jokes.
//
// Nothing personal is ever loaded into someone else's conversation. That is
// enforced here, by which key each read uses -- not by asking the model
// nicely.

const (
	// maxMemoryContentLen bounds one written fact. mem0 rewrites what it is
	// given into its own phrasing, so this is a guard against a model
	// pasting an entire conversation into a single "fact", not a display
	// limit.
	maxMemoryContentLen = 600
)

// LoadMemoryContext renders what the agent knows about this person and this
// server as a system-prompt section, or "" when there is nothing.
//
// Read unprompted, before the model has said anything, exactly as the old
// implementation was: memory the model has to remember to look up is memory it
// will forget to look up. The recall tool exists on top of this for the deeper
// "what did they say about X" case, not instead of it.
func LoadMemoryContext(ctx context.Context, mem *Memory, guildID, userID int64) (string, error) {
	if !mem.Enabled() {
		return "", nil
	}

	personal, err := mem.List(ctx, UserKey(userID), memoryContextLimit)
	if err != nil {
		return "", fmt.Errorf("memory: failed to load personal memories: %w", err)
	}
	shared, err := mem.List(ctx, SharedKey(guildID), memoryContextLimit)
	if err != nil {
		return "", fmt.Errorf("memory: failed to load shared memories: %w", err)
	}
	if len(personal) == 0 && len(shared) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("## What you remember\n")
	if len(personal) > 0 {
		b.WriteString("\nAbout this specific person, from past conversations:\n")
		for _, r := range personal {
			fmt.Fprintf(&b, "- %s [id: %s]\n", r.Memory, r.ID)
		}
	}
	if len(shared) > 0 {
		b.WriteString("\nAbout this server generally:\n")
		for _, r := range shared {
			fmt.Fprintf(&b, "- %s [id: %s]\n", r.Memory, r.ID)
		}
	}
	b.WriteString("\nThese are yours to correct. If something here is wrong or out of date, " +
		"update or forget it rather than working around it.\n")
	return b.String(), nil
}

func memoryTools() []Tool {
	return []Tool{
		{Type: "function", Function: ToolFunction{
			Name: "remember",
			Description: "Save something worth carrying into future conversations: a preference, what someone does, a running joke, a correction they made. " +
				"Use \"personal\" for something about the person you're talking to -- it follows them into every server, and only they can trigger it. " +
				"Use \"shared\" only for something true of the whole server that anyone may hear back. " +
				"Never save anything sensitive: addresses, contact details, anything said in confidence, anything about a third party who isn't here. " +
				"Don't narrate that you're saving it, and don't do it on every message -- only when it would actually matter next time.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"scope": {"type": "string", "enum": ["personal", "shared"]},
					"content": {"type": "string", "description": "one short self-contained fact, phrased so it still makes sense months from now"}
				},
				"required": ["scope", "content"]
			}`),
		}},
		{Type: "function", Function: ToolFunction{
			Name: "recall",
			Description: "Search your memory by meaning for things not already in your context -- older conversations, or a detail you only half remember. " +
				"Use it when someone refers to something you should know but can't see above. Searches only what you know about the person you're talking to, plus this server's shared memory.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "what you're trying to remember, in plain words"},
					"scope": {"type": "string", "enum": ["personal", "shared"], "description": "defaults to personal"}
				},
				"required": ["query"]
			}`),
		}},
		{Type: "function", Function: ToolFunction{
			Name: "update_memory",
			Description: "Correct a memory in place, keeping its history, when something you remembered has changed or was wrong. " +
				"Prefer this over forgetting and re-remembering: it keeps the correction traceable. Use the id shown next to the memory.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"memory_id": {"type": "string"},
					"content": {"type": "string", "description": "the corrected fact, in full"}
				},
				"required": ["memory_id", "content"]
			}`),
		}},
		{Type: "function", Function: ToolFunction{
			Name: "forget",
			Description: "Delete a memory by its id. Use it when someone asks you to forget something, or when a memory turns out to be wrong and has no corrected version. " +
				"Always honour a request to forget something about the person asking.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"memory_id": {"type": "string"}
				},
				"required": ["memory_id"]
			}`),
		}},
	}
}

// dispatchMemoryTool handles the memory tools. guildID and userID come from
// the request, never from the model: "personal" always means the person who
// triggered this conversation, so no message can be crafted to make the bot
// write a memory against somebody else, or read one belonging to them.
func dispatchMemoryTool(ctx context.Context, mem *Memory, guildID, userID int64, name, argsJSON string) (string, error) {
	if !mem.Enabled() {
		return marshal(map[string]any{"error": "memory is not configured"})
	}

	switch name {
	case "remember":
		var args struct {
			Scope   string `json:"scope"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("remember: bad arguments: %w", err)
		}
		content := strings.TrimSpace(args.Content)
		if content == "" {
			return "", fmt.Errorf("remember: content is empty")
		}
		if len(content) > maxMemoryContentLen {
			content = content[:maxMemoryContentLen]
		}

		key, err := scopeKey(args.Scope, guildID, userID)
		if err != nil {
			return "", err
		}

		// Fire and forget. A write costs mem0 an extraction round-trip of
		// about eight seconds; blocking a Discord reply on it is the
		// difference between the bot feeling present and feeling broken. The
		// model is told it worked because from its side it did -- the write
		// is queued and only infrastructure failure stops it, which is logged
		// where somebody can act on it.
		mem.AddAsync(key, content, map[string]any{
			"scope":    args.Scope,
			"guild_id": fmt.Sprint(guildID),
			"source":   "conversation",
		})
		return marshal(map[string]any{"remembered": true})

	case "recall":
		var args struct {
			Query string `json:"query"`
			Scope string `json:"scope"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("recall: bad arguments: %w", err)
		}
		if args.Scope == "" {
			args.Scope = "personal"
		}
		key, err := scopeKey(args.Scope, guildID, userID)
		if err != nil {
			return "", err
		}

		found, err := mem.Search(ctx, key, args.Query, memorySearchLimit)
		if err != nil {
			return "", err
		}
		out := make([]map[string]any, 0, len(found))
		for _, r := range found {
			out = append(out, map[string]any{"memory_id": r.ID, "memory": r.Memory})
		}
		return marshal(map[string]any{"results": out})

	case "update_memory":
		var args struct {
			MemoryID string `json:"memory_id"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("update_memory: bad arguments: %w", err)
		}
		content := strings.TrimSpace(args.Content)
		if len(content) > maxMemoryContentLen {
			content = content[:maxMemoryContentLen]
		}
		if err := mem.owns(ctx, args.MemoryID, guildID, userID); err != nil {
			return marshal(map[string]any{"error": err.Error()})
		}
		if err := mem.Update(ctx, args.MemoryID, content); err != nil {
			return "", err
		}
		return marshal(map[string]any{"updated": true})

	case "forget":
		var args struct {
			MemoryID string `json:"memory_id"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("forget: bad arguments: %w", err)
		}
		if err := mem.owns(ctx, args.MemoryID, guildID, userID); err != nil {
			return marshal(map[string]any{"error": err.Error()})
		}
		if err := mem.Delete(ctx, args.MemoryID); err != nil {
			return "", err
		}
		return marshal(map[string]any{"forgotten": true})

	default:
		return "", fmt.Errorf("ai: unknown memory tool %q", name)
	}
}

func scopeKey(scope string, guildID, userID int64) (string, error) {
	switch scope {
	case "personal":
		return UserKey(userID), nil
	case "shared":
		return SharedKey(guildID), nil
	default:
		return "", fmt.Errorf("scope must be \"personal\" or \"shared\", got %q", scope)
	}
}

// owns checks that a memory id the model supplied belongs to this
// conversation before it is edited or deleted. Memory ids are UUIDs handed to
// the model in its own context, but a model that hallucinates or is talked
// into quoting somebody else's id must not be able to reach through it -- and
// mem0's update and delete endpoints take a bare id with no scope of their
// own.
func (m *Memory) owns(ctx context.Context, id string, guildID, userID int64) error {
	if m == nil {
		return fmt.Errorf("memory is not configured")
	}
	for _, key := range []string{UserKey(userID), SharedKey(guildID)} {
		records, err := m.List(ctx, key, maxForgetPerPass)
		if err != nil {
			return fmt.Errorf("could not verify that memory: %w", err)
		}
		for _, r := range records {
			if r.ID == id {
				return nil
			}
		}
	}
	return fmt.Errorf("no memory with id %q belongs to this conversation", id)
}
