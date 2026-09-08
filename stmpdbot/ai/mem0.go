package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// This file is the persona's long-term memory. It talks to a self-hosted mem0
// instance shared with other applications on the same box, which is the whole
// reason AgentID exists: segregation there is by agent_id on the memory
// itself, not by network or database. Every call made here carries ours, so
// nothing this bot writes is visible to the "wizard" or "hermes" agents, and
// nothing they write is visible here.
//
// The old implementation kept memories in this bot's own postgres, scoped to
// (guild, user). That is why it could not do the thing that makes memory feel
// like a person rather than a database: recognise someone across servers, and
// find a fact by meaning rather than by exact scope. mem0 is a vector store
// with an LLM in front of it -- it decides on write whether a new fact ADDs,
// UPDATEs an existing one, or is already known, which is what keeps a memory
// current without anyone auditing it.

const (
	// memorySearchLimit is how many memories a semantic search returns. This
	// lands in the system prompt, so it is a token budget as much as a
	// relevance one.
	memorySearchLimit = 8

	// memoryContextLimit is how many of a person's memories are loaded
	// unprompted, before the model has said anything. Recall by search is
	// better, but this is what makes the bot open a conversation already
	// knowing who it is talking to.
	memoryContextLimit = 12

	// maxForgetPerPass is the page size DeleteAllForUser works through. It
	// exists because a purge must finish -- a cap that silently leaves rows
	// behind would make /forgetme a lie.
	maxForgetPerPass = 100
)

// Memory is a mem0 client. A nil *Memory is valid and means the feature is
// off: every method no-ops, so the agent runs exactly as it did before mem0
// existed rather than failing every request.
type Memory struct {
	// Two clients, because reads and writes have nothing in common here. A
	// search is ~1s and sits in front of a reply somebody is waiting on. A
	// write runs mem0's own fact-extraction LLM call and measures ~8s, which
	// no interactive reply should ever block on -- see AddAsync.
	readClient  *http.Client
	writeClient *http.Client
	baseURL     string
	apiKey      string
	agentID     string
}

// NewMemory returns nil when baseURL is empty, which is what turns the feature
// off. Callers are expected to hold a possibly-nil *Memory and call through it
// regardless.
func NewMemory(baseURL, apiKey, agentID string) *Memory {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	return &Memory{
		// Better to answer without memory than to make someone wait for it.
		readClient:  &http.Client{Timeout: 15 * time.Second},
		writeClient: &http.Client{Timeout: 60 * time.Second},
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      apiKey,
		agentID:     agentID,
	}
}

// Enabled reports whether memory is configured. Kept as a method so a nil
// receiver answers it rather than panicking at the call site.
func (m *Memory) Enabled() bool { return m != nil }

// SetWriteTimeout widens the leash on writes, for callers that hand mem0 a
// whole batch of messages at once rather than a single fact.
//
// The default is sized for the interactive path, where one short assertion
// extracts in about eight seconds. A batch of forty real messages is a much
// larger prompt and routinely takes several times that -- the history backfill
// lost batches to the default before this existed. Not for the request path:
// nothing a person is waiting on should be given a longer deadline.
func (m *Memory) SetWriteTimeout(d time.Duration) {
	if m == nil || d <= 0 {
		return
	}
	m.writeClient.Timeout = d
}

// Record is one remembered fact.
type Record struct {
	ID        string         `json:"id"`
	Memory    string         `json:"memory"`
	UserID    string         `json:"user_id,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Score     float64        `json:"score,omitempty"`
	CreatedAt string         `json:"created_at,omitempty"`
	UpdatedAt string         `json:"updated_at,omitempty"`
}

// UserKey is the mem0 user_id for a Discord account. It is deliberately not
// the bare snowflake: the instance is shared, and "421608483629301772" on its
// own says nothing about which product it belongs to.
//
// It carries no guild. That is the point of the rewrite -- the same person in
// two servers is one person, and the bot should not have to be reintroduced.
func UserKey(userID int64) string {
	return "discord:" + strconv.FormatInt(userID, 10)
}

// SharedKey is the pseudo-user holding facts about the community itself
// rather than about any one person: inside jokes, what the server is for,
// who runs it. Everyone's conversation can reach these; nobody's private
// facts live here.
func SharedKey(guildID int64) string {
	return "discord-guild:" + strconv.FormatInt(guildID, 10)
}

// Mem0Message is one turn handed to mem0's extraction. Exported because the
// backfill script feeds it whole runs of real dialogue, which is what its
// extraction is actually good at.
type Mem0Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type addRequest struct {
	Messages []Mem0Message  `json:"messages"`
	UserID   string         `json:"user_id"`
	AgentID  string         `json:"agent_id"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type searchRequest struct {
	Query   string         `json:"query"`
	Filters map[string]any `json:"filters,omitempty"`
	TopK    int            `json:"top_k,omitempty"`
}

type resultsResponse struct {
	Results []Record `json:"results"`
}

// Add stores a fact. mem0 runs its own extraction over what it is given and
// decides whether this is new, an update to something already known, or
// nothing worth keeping -- so the caller does not have to check first, and
// re-asserting a fact months later refreshes it in place instead of
// duplicating it.
func (m *Memory) Add(ctx context.Context, userKey, content string, metadata map[string]any) ([]Record, error) {
	if m == nil {
		return nil, nil
	}

	meta := map[string]any{}
	for k, v := range metadata {
		meta[k] = v
	}
	// Every write stamps when the fact was last seen. Nothing reads this yet;
	// it is what a future decay pass needs to tell a fact that is still true
	// from one nobody has mentioned in a year.
	meta["last_seen"] = time.Now().UTC().Format(time.RFC3339)

	var out resultsResponse
	err := m.do(ctx, http.MethodPost, "/memories", addRequest{
		Messages: []Mem0Message{{Role: "user", Content: content}},
		UserID:   userKey,
		AgentID:  m.agentID,
		Metadata: meta,
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.Results, nil
}

// AddConversation stores a whole exchange rather than a single assertion,
// which is how the backfill and the nightly digest feed it: mem0's extraction
// is much better at pulling durable facts out of real dialogue than out of a
// sentence somebody wrote for it.
func (m *Memory) AddConversation(ctx context.Context, userKey string, messages []Mem0Message, metadata map[string]any) ([]Record, error) {
	if m == nil || len(messages) == 0 {
		return nil, nil
	}

	meta := map[string]any{}
	for k, v := range metadata {
		meta[k] = v
	}
	meta["last_seen"] = time.Now().UTC().Format(time.RFC3339)

	var out resultsResponse
	err := m.do(ctx, http.MethodPost, "/memories", addRequest{
		Messages: messages, UserID: userKey, AgentID: m.agentID, Metadata: meta,
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.Results, nil
}

// AddAsync stores a fact without making anyone wait for it. mem0's write path
// runs a fact-extraction LLM call and measures around eight seconds; spending
// that inside a Discord reply -- on top of the model round-trips already in
// flight -- is the difference between the bot feeling present and feeling
// broken.
//
// The context is deliberately detached from the request that triggered it:
// the reply will be sent and its context cancelled long before this finishes.
// Failure is logged and nothing else, because by the time it happens there is
// no longer a conversation to apologise to.
func (m *Memory) AddAsync(userKey, content string, metadata map[string]any) {
	if m == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if _, err := m.Add(ctx, userKey, content, metadata); err != nil {
			slog.Error("mem0: failed to store a memory",
				slog.String("user_key", userKey), slog.Any("err", err))
		}
	}()
}

// Search finds memories by meaning. This is the call that makes the feature
// worth having: "what does he like" reaches a fact recorded as "prefers
// melodic techno over big room" without either sharing a word.
func (m *Memory) Search(ctx context.Context, userKey, query string, limit int) ([]Record, error) {
	if m == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = memorySearchLimit
	}

	filters := map[string]any{"agent_id": m.agentID}
	if userKey != "" {
		filters["user_id"] = userKey
	}

	var out resultsResponse
	err := m.do(ctx, http.MethodPost, "/search", searchRequest{
		Query: query, Filters: filters, TopK: limit,
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.Results, nil
}

// List returns a person's memories without a query, newest first, for the
// unprompted context load and for "what do you remember about me".
func (m *Memory) List(ctx context.Context, userKey string, limit int) ([]Record, error) {
	if m == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = memoryContextLimit
	}

	q := url.Values{}
	q.Set("user_id", userKey)
	q.Set("agent_id", m.agentID)
	q.Set("top_k", strconv.Itoa(limit))

	var out resultsResponse
	if err := m.do(ctx, http.MethodGet, "/memories?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// Update rewrites one memory in place, keeping its id and history. mem0 keeps
// a per-memory audit trail, so a correction stays traceable to what it
// replaced.
func (m *Memory) Update(ctx context.Context, id, content string) error {
	if m == nil {
		return nil
	}
	return m.do(ctx, http.MethodPut, "/memories/"+url.PathEscape(id),
		map[string]any{"text": content}, nil)
}

// Delete removes one memory.
func (m *Memory) Delete(ctx context.Context, id string) error {
	if m == nil {
		return nil
	}
	return m.do(ctx, http.MethodDelete, "/memories/"+url.PathEscape(id), nil, nil)
}

// DeleteAllForUser is what /forgetme calls. It lists and deletes one at a
// time rather than using mem0's bulk DELETE /memories, for two reasons: that
// endpoint requires an admin role this bot's API key deliberately does not
// have, and a bulk delete keyed on agent_id is one typo away from erasing
// every memory this application holds for everyone.
//
// Scoped by agent_id throughout, so a person purging themselves here does not
// touch what the other applications on this shared instance hold about them.
// That is not this bot's data to delete.
func (m *Memory) DeleteAllForUser(ctx context.Context, userKey string) (int, error) {
	if m == nil {
		return 0, nil
	}

	var deleted int
	// Loop: List is capped, and somebody with a long history has more
	// memories than one page.
	for {
		records, err := m.List(ctx, userKey, maxForgetPerPass)
		if err != nil {
			return deleted, err
		}
		if len(records) == 0 {
			return deleted, nil
		}
		for _, r := range records {
			if err := m.Delete(ctx, r.ID); err != nil {
				return deleted, err
			}
			deleted++
		}
		if len(records) < maxForgetPerPass {
			return deleted, nil
		}
	}
}

// History returns how one memory changed over time.
func (m *Memory) History(ctx context.Context, id string) ([]map[string]any, error) {
	if m == nil {
		return nil, nil
	}
	var out []map[string]any
	if err := m.do(ctx, http.MethodGet, "/memories/"+url.PathEscape(id)+"/history", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Memory) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mem0: failed to encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("mem0: failed to build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// X-API-Key, not Authorization: the Bearer scheme on this server is for
	// dashboard JWTs and rejects an API key with "Invalid or expired token".
	req.Header.Set("X-API-Key", m.apiKey)

	client := m.readClient
	if method == http.MethodPost && strings.HasPrefix(path, "/memories") {
		client = m.writeClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mem0: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("mem0: failed to read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mem0: %s %s returned %d: %s", method, path, resp.StatusCode, truncate(string(data), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("mem0: failed to decode response: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
