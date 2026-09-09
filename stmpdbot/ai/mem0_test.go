package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMem0 records what the client sent, so the tests can assert on the thing
// that actually matters here: every call is scoped to this application's
// agent_id and to the right person.
type fakeMem0 struct {
	mu       sync.Mutex
	requests []recordedRequest
	records  map[string][]Record // keyed by user_id
}

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

func newFakeMem0(t *testing.T) (*fakeMem0, *Memory) {
	t.Helper()
	f := &fakeMem0{records: map[string][]Record{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body,
		})
		records := f.records[r.URL.Query().Get("user_id")]
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/memories":
			_ = json.NewEncoder(w).Encode(resultsResponse{Results: records})
		case r.Method == http.MethodPost && r.URL.Path == "/search":
			// Answer from the seeded records, keyed by the filter the client
			// sent -- otherwise every search "finds" something and the
			// empty-result path can never be tested.
			var hits []Record
			if filters, ok := body["filters"].(map[string]any); ok {
				if key, ok := filters["user_id"].(string); ok {
					f.mu.Lock()
					hits = f.records[key]
					f.mu.Unlock()
				}
			}
			_ = json.NewEncoder(w).Encode(resultsResponse{Results: hits})
		case r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(resultsResponse{Results: []Record{{ID: "new-id", Memory: "stored"}}})
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	return f, NewMemory(srv.URL, "test-key", "garrixbot")
}

func (f *fakeMem0) last() recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func (f *fakeMem0) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// The instance is shared with other applications. If agent_id ever stops being
// attached to a call, this bot starts reading and writing their memories.
func TestMemoryAlwaysScopesToItsAgentID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("writes carry it", func(t *testing.T) {
		t.Parallel()
		f, m := newFakeMem0(t)

		if _, err := m.Add(ctx, UserKey(42), "likes melodic techno", nil); err != nil {
			t.Fatal(err)
		}
		if got := f.last().Body["agent_id"]; got != "garrixbot" {
			t.Errorf("agent_id = %v, want garrixbot", got)
		}
	})

	t.Run("searches filter on it", func(t *testing.T) {
		t.Parallel()
		f, m := newFakeMem0(t)

		if _, err := m.Search(ctx, UserKey(42), "music", 5); err != nil {
			t.Fatal(err)
		}
		filters, ok := f.last().Body["filters"].(map[string]any)
		if !ok {
			t.Fatalf("no filters sent: %+v", f.last().Body)
		}
		if filters["agent_id"] != "garrixbot" {
			t.Errorf("filters = %+v, want agent_id garrixbot", filters)
		}
		if filters["user_id"] != "discord:42" {
			t.Errorf("filters = %+v, want the caller's user_id", filters)
		}
	})

	t.Run("listing filters on it", func(t *testing.T) {
		t.Parallel()
		f, m := newFakeMem0(t)

		if _, err := m.List(ctx, UserKey(42), 5); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(f.last().Query, "agent_id=garrixbot") {
			t.Errorf("query = %q, want agent_id", f.last().Query)
		}
	})
}

// A person is one person everywhere. The old implementation keyed on
// (guild, user), which is why the bot had to be reintroduced in every server.
func TestUserKeyIsGuildIndependent(t *testing.T) {
	t.Parallel()

	if got, want := UserKey(421608483629301772), "discord:421608483629301772"; got != want {
		t.Errorf("UserKey() = %q, want %q", got, want)
	}
	if UserKey(1) == SharedKey(1) {
		t.Error("a person and a server must not collide on the same key")
	}
	if !strings.HasPrefix(SharedKey(690950056202731521), "discord-guild:") {
		t.Errorf("SharedKey() = %q, want a distinct namespace", SharedKey(690950056202731521))
	}
}

// A nil *Memory is how the feature is switched off. Every method has to
// tolerate it, or an unconfigured deployment panics on the first mention.
func TestNilMemoryIsUsable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var m *Memory
	if m.Enabled() {
		t.Error("a nil Memory must report itself disabled")
	}

	if _, err := m.Add(ctx, "u", "x", nil); err != nil {
		t.Errorf("Add: %v", err)
	}
	if got, err := m.Search(ctx, "u", "q", 5); err != nil || got != nil {
		t.Errorf("Search: %v %v", got, err)
	}
	if got, err := m.List(ctx, "u", 5); err != nil || got != nil {
		t.Errorf("List: %v %v", got, err)
	}
	if err := m.Update(ctx, "id", "x"); err != nil {
		t.Errorf("Update: %v", err)
	}
	if err := m.Delete(ctx, "id"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if n, err := m.DeleteAllForUser(ctx, "u"); err != nil || n != 0 {
		t.Errorf("DeleteAllForUser: %d %v", n, err)
	}
	m.AddAsync("u", "x", nil) // must not panic

	if ctxStr, err := LoadMemoryContext(ctx, m, 1, 2, "anything"); err != nil || ctxStr != "" {
		t.Errorf("LoadMemoryContext: %q %v", ctxStr, err)
	}
}

// /forgetme has to actually finish. A purge that stops at one page would tell
// somebody their data is gone while it is not.
func TestDeleteAllForUserPagesUntilEmpty(t *testing.T) {
	t.Parallel()

	var (
		mu        sync.Mutex
		remaining = maxForgetPerPass + 7
		deletes   int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodDelete {
			deletes++
			remaining--
			_, _ = w.Write([]byte(`{}`))
			return
		}

		n := min(remaining, maxForgetPerPass)
		records := make([]Record, n)
		for i := range records {
			records[i] = Record{ID: "id", Memory: "x"}
		}
		_ = json.NewEncoder(w).Encode(resultsResponse{Results: records})
	}))
	t.Cleanup(srv.Close)

	m := NewMemory(srv.URL, "k", "garrixbot")
	got, err := m.DeleteAllForUser(context.Background(), UserKey(1))
	if err != nil {
		t.Fatal(err)
	}
	if want := maxForgetPerPass + 7; got != want {
		t.Errorf("deleted %d, want %d -- the purge stopped early", got, want)
	}
}

// Personal memories must never surface in somebody else's conversation. The
// guarantee is which key each read uses, so this asserts on the keys.
func TestLoadMemoryContextReadsOnlyTheCallerAndTheGuild(t *testing.T) {
	t.Parallel()

	f, m := newFakeMem0(t)
	f.records["discord:42"] = []Record{{ID: "a", Memory: "likes melodic techno"}}
	f.records["discord-guild:7"] = []Record{{ID: "b", Memory: "the server is about STMPD RCRDS"}}

	out, err := LoadMemoryContext(context.Background(), m, 7, 42, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "likes melodic techno") || !strings.Contains(out, "STMPD RCRDS") {
		t.Errorf("context missing memories:\n%s", out)
	}
	if !strings.Contains(out, "[id: a]") {
		t.Error("ids must be shown, or the model cannot correct or forget anything")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		user := req.Query
		if !strings.Contains(user, "discord%3A42") && !strings.Contains(user, "discord-guild%3A7") {
			t.Errorf("read a key belonging to neither the caller nor the guild: %q", req.Query)
		}
	}
}

func TestLoadMemoryContextEmptyWhenNothingRemembered(t *testing.T) {
	t.Parallel()

	_, m := newFakeMem0(t)
	out, err := LoadMemoryContext(context.Background(), m, 7, 42, "")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("got %q, want empty so nothing is added to the system prompt", out)
	}
}

// The model supplies memory ids, and mem0's update/delete endpoints take a
// bare id with no scope of their own. A hallucinated or quoted id from
// somebody else's memory must not reach through.
func TestMemoryOwnershipGate(t *testing.T) {
	t.Parallel()

	f, m := newFakeMem0(t)
	f.records["discord:42"] = []Record{{ID: "mine", Memory: "x"}}
	f.records["discord-guild:7"] = []Record{{ID: "ours", Memory: "y"}}

	ctx := context.Background()

	if err := m.owns(ctx, "mine", 7, 42); err != nil {
		t.Errorf("own memory rejected: %v", err)
	}
	if err := m.owns(ctx, "ours", 7, 42); err != nil {
		t.Errorf("shared memory rejected: %v", err)
	}
	if err := m.owns(ctx, "someone-elses", 7, 42); err == nil {
		t.Error("an id belonging to neither the caller nor the guild was accepted")
	}

	// And the tools must refuse rather than pass it through to mem0.
	out, err := dispatchMemoryTool(ctx, m, 7, 42, "forget", `{"memory_id":"someone-elses"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("forget on a foreign id returned %s, want an error result", out)
	}
}

func TestRememberScopesToTheRightKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("personal writes against the caller", func(t *testing.T) {
		t.Parallel()
		f, m := newFakeMem0(t)

		if _, err := dispatchMemoryTool(ctx, m, 7, 42, "remember", `{"scope":"personal","content":"loves DnB"}`); err != nil {
			t.Fatal(err)
		}
		waitFor(t, f, 1)
		if got := f.last().Body["user_id"]; got != "discord:42" {
			t.Errorf("user_id = %v, want the caller", got)
		}
	})

	t.Run("shared writes against the guild", func(t *testing.T) {
		t.Parallel()
		f, m := newFakeMem0(t)

		if _, err := dispatchMemoryTool(ctx, m, 7, 42, "remember", `{"scope":"shared","content":"server is for STMPD"}`); err != nil {
			t.Fatal(err)
		}
		waitFor(t, f, 1)
		if got := f.last().Body["user_id"]; got != "discord-guild:7" {
			t.Errorf("user_id = %v, want the guild", got)
		}
	})

	t.Run("an unknown scope is refused", func(t *testing.T) {
		t.Parallel()
		_, m := newFakeMem0(t)

		if _, err := dispatchMemoryTool(ctx, m, 7, 42, "remember", `{"scope":"everyone","content":"x"}`); err == nil {
			t.Error("an invented scope was accepted")
		}
	})
}

// remember writes asynchronously so a reply is not held up for mem0's
// extraction round-trip, so the assertion has to wait for it to land.
func waitFor(t *testing.T, f *fakeMem0, want int) {
	t.Helper()
	for range 100 {
		if f.count() >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no request arrived after waiting; got %d, want %d", f.count(), want)
}

// The bug this guards: every one of the first 11,243 memories was written
// "personal", so the shared pool was never used and nothing the bot learned
// was ever available to anyone but the person who said it. A fact about a
// track belongs to everyone; only facts about a person are theirs.
func TestRecallSearchesBothScopesByDefault(t *testing.T) {
	t.Parallel()

	f, m := newFakeMem0(t)
	ctx := context.Background()

	if _, err := dispatchMemoryTool(ctx, m, 7, 42, "recall", `{"query":"breakaway lasers"}`); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var searchedPersonal, searchedShared bool
	for _, req := range f.requests {
		filters, ok := req.Body["filters"].(map[string]any)
		if !ok {
			continue
		}
		switch filters["user_id"] {
		case "discord:42":
			searchedPersonal = true
		case "discord-guild:7":
			searchedShared = true
		}
	}
	if !searchedPersonal || !searchedShared {
		t.Errorf("recall searched personal=%v shared=%v; a question about a track is not about the asker",
			searchedPersonal, searchedShared)
	}
}

func TestRecallNarrowsWhenAsked(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for _, tc := range []struct{ scope, wantKey string }{
		{"personal", "discord:42"},
		{"shared", "discord-guild:7"},
	} {
		f, m := newFakeMem0(t)
		if _, err := dispatchMemoryTool(ctx, m, 7, 42, "recall",
			`{"query":"x","scope":"`+tc.scope+`"}`); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		filters, _ := f.requests[0].Body["filters"].(map[string]any)
		f.mu.Unlock()
		if filters["user_id"] != tc.wantKey {
			t.Errorf("scope %q searched %v, want %q", tc.scope, filters["user_id"], tc.wantKey)
		}
	}
}

// An empty result has to say so out loud: a bare empty list reads as "nothing
// to add" and the model fills the silence by inventing a colour.
func TestRecallSaysWhenItFoundNothing(t *testing.T) {
	t.Parallel()

	_, m := newFakeMem0(t)
	out, err := dispatchMemoryTool(context.Background(), m, 7, 42, "recall", `{"query":"nothing here"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "rather than guessing") {
		t.Errorf("empty recall returned %s; it must tell the model not to guess", out)
	}
}

func TestRecallRejectsAnUnknownScope(t *testing.T) {
	t.Parallel()

	_, m := newFakeMem0(t)
	if _, err := dispatchMemoryTool(context.Background(), m, 7, 42, "recall",
		`{"query":"x","scope":"everyone"}`); err == nil {
		t.Error("an invented scope was accepted")
	}
}

// The bug: the context load listed memories instead of searching them, on the
// assumption that listing came back newest-first. mem0 returns whatever order
// the vector store yields, so once the shared pool reached 900 entries the bot
// was handed twelve arbitrary facts per message. Told repeatedly who HALŌ
// were, it kept answering that it had never heard of them -- the memory was
// there, it was just never in the twelve.
func TestLoadMemoryContextSearchesRatherThanListing(t *testing.T) {
	t.Parallel()

	f, m := newFakeMem0(t)
	// Seeded so a listing would return them, but the assertion is about which
	// endpoint gets called.
	f.records["discord-guild:7"] = []Record{{ID: "s1", Memory: "HALO is DubVision, Third Party and Matisse & Sadko"}}
	f.records["discord:42"] = []Record{{ID: "p1", Memory: "prefers melodic techno"}}

	if _, err := LoadMemoryContext(context.Background(), m, 7, 42, "who is HALO?"); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var searched, listed int
	for _, req := range f.requests {
		switch {
		case req.Method == http.MethodPost && req.Path == "/search":
			searched++
			if req.Body["query"] != "who is HALO?" {
				t.Errorf("searched for %v, want the message being answered", req.Body["query"])
			}
		case req.Method == http.MethodGet && req.Path == "/memories":
			listed++
		}
	}
	if searched != 2 {
		t.Errorf("made %d searches, want one per scope -- a blind listing cannot surface the relevant fact", searched)
	}
	if listed != 0 {
		t.Errorf("made %d listings; listing returns arbitrary order and does not scale past a few dozen memories", listed)
	}
}

// An image posted with no words has nothing to search on. Falling back to a
// listing is fine there; falling over is not.
func TestLoadMemoryContextFallsBackWhenThereIsNoQuery(t *testing.T) {
	t.Parallel()

	f, m := newFakeMem0(t)
	f.records["discord:42"] = []Record{{ID: "p1", Memory: "prefers melodic techno"}}

	out, err := LoadMemoryContext(context.Background(), m, 7, 42, "   ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "melodic techno") {
		t.Errorf("got %q, want the listing fallback to still produce context", out)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if req.Path == "/search" {
			t.Error("searched with an empty query instead of falling back to a listing")
		}
	}
}
