package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/stmpdbot/ai"
)

type server struct {
	queries *db.Queries
	client  *ai.Client
	// memory is the shared mem0 instance. Nil when memory is unconfigured,
	// which every call through it tolerates -- the persona then simply has no
	// long-term memory rather than failing the request.
	memory *ai.Memory
	secret string
}

type respondMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Attachments is what the message had hanging off it in Discord --
	// images, stickers, voice notes, files. The bot sends the metadata only;
	// this service fetches and converts, because what a model can be shown is
	// this side's business. See ai/media.go.
	Attachments []ai.Attachment `json:"attachments,omitempty"`
}

type respondRequest struct {
	GuildID  int64            `json:"guild_id"`
	UserID   int64            `json:"user_id"`
	Messages []respondMessage `json:"messages"`
}

type respondResponse struct {
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (s *server) ListenAndServe(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /respond", s.handleRespond)
	mux.HandleFunc("POST /forget", s.handleForget)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           s.auth(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// auth compares the presented token in constant time, same pattern as
// stmpdbot/internalapi.go's internalAuth. /health is exempt so the container
// HEALTHCHECK doesn't need the secret.
func (s *server) auth(next http.Handler) http.Handler {
	want := []byte(s.secret)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get("X-Internal-Token"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleRespond(w http.ResponseWriter, r *http.Request) {
	var req respondRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, respondResponse{Error: "bad request body"})
		return
	}
	if req.GuildID == 0 || req.UserID == 0 || len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, respondResponse{Error: "guild_id, user_id and messages are required"})
		return
	}

	ctx := r.Context()

	// Memory is read here, unconditionally, and folded into the system
	// prompt -- never left to the model to proactively "recall", so it
	// can't forget to check. A failure to load it degrades to answering
	// without memory rather than failing the whole reply.
	memoryCtx, err := ai.LoadMemoryContext(ctx, s.memory, req.GuildID, req.UserID)
	if err != nil {
		slog.Error("agent: failed to load memory context",
			slog.Int64("guild_id", req.GuildID), slog.Int64("user_id", req.UserID), slog.Any("err", err))
	}

	systemPrompt := ai.SystemPrompt()
	if memoryCtx != "" {
		systemPrompt += "\n\n---\n\n" + memoryCtx
	}

	// One fetcher per request: its image budget is per conversation. Spend it
	// newest-first -- req.Messages runs oldest to newest, and the picture
	// somebody is actually asking about is the one they just posted, not the
	// meme six hops up the reply chain.
	fetcher := ai.NewFetcher()
	parts := make([][]ai.Part, len(req.Messages))
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		// Only a user turn carries media. An assistant turn is something this
		// service said, and it says text.
		if m.Role == "user" && len(m.Attachments) > 0 {
			parts[i] = fetcher.BuildParts(ctx, m.Content, m.Attachments)
		}
	}

	messages := make([]ai.Message, 0, len(req.Messages)+1)
	messages = append(messages, ai.Message{Role: "system", Content: systemPrompt})
	for i, m := range req.Messages {
		messages = append(messages, ai.Message{Role: m.Role, Content: m.Content, Parts: parts[i]})
	}

	content, err := s.client.Respond(ctx, s.queries, s.memory, req.GuildID, req.UserID, messages)
	if err != nil {
		slog.Error("agent: failed to generate a response",
			slog.Int64("guild_id", req.GuildID), slog.Int64("user_id", req.UserID), slog.Any("err", err))
		writeJSON(w, http.StatusInternalServerError, respondResponse{Error: "failed to generate a response"})
		return
	}

	slog.Info("agent: responded", slog.Int64("guild_id", req.GuildID), slog.Int64("user_id", req.UserID))
	writeJSON(w, http.StatusOK, respondResponse{Content: content})
}

type forgetRequest struct {
	UserID int64 `json:"user_id"`
}

type forgetResponse struct {
	Deleted int    `json:"deleted"`
	Error   string `json:"error,omitempty"`
}

// handleForget backs the bot's /forgetme command. It lives here rather than in
// the bot because the bot deliberately holds no mem0 credentials, the same
// reason it holds no LLM API key.
//
// It erases only what THIS application remembers about the person: the delete
// is scoped by agent_id, so memories the other agents on the shared instance
// hold about the same human are untouched. Those are not this bot's to delete.
func (s *server) handleForget(w http.ResponseWriter, r *http.Request) {
	var req forgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, forgetResponse{Error: "bad request body"})
		return
	}
	if req.UserID == 0 {
		writeJSON(w, http.StatusBadRequest, forgetResponse{Error: "user_id is required"})
		return
	}
	if !s.memory.Enabled() {
		writeJSON(w, http.StatusOK, forgetResponse{Deleted: 0})
		return
	}

	deleted, err := s.memory.DeleteAllForUser(r.Context(), ai.UserKey(req.UserID))
	if err != nil {
		slog.Error("agent: failed to forget a user", slog.Int64("user_id", req.UserID), slog.Any("err", err))
		// Report the partial count: a purge that half-finished must not look
		// like one that did nothing, or the person will be told their data is
		// gone when some of it is not.
		writeJSON(w, http.StatusInternalServerError, forgetResponse{Deleted: deleted, Error: "failed to delete every memory"})
		return
	}

	slog.Info("agent: forgot a user on request", slog.Int64("user_id", req.UserID), slog.Int("deleted", deleted))
	writeJSON(w, http.StatusOK, forgetResponse{Deleted: deleted})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
