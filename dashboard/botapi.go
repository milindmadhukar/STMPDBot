package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// BotAPI is the dashboard's client for the bot's internal API. It is the only
// route to live Discord state; the dashboard holds no bot token and never calls
// Discord directly outside the OAuth handshake.
//
// Every method is best-effort by design. The bot is redeployed automatically on
// every push to main, so it being briefly unreachable is routine, not
// exceptional. Callers render IDs and a degradation banner rather than a 500 --
// a dashboard that dies whenever the bot restarts is not useful.
type BotAPI struct {
	baseURL string
	secret  string
	client  *http.Client
	// resolveClient carries a longer deadline than client. Every other call is a
	// cached metadata GET the bot answers from its disgo cache in milliseconds;
	// /users/resolve is the only one that fans out to Discord REST, up to two
	// calls per uncached id. A cold cache and a 50-row page therefore blew the
	// 5s budget, ResolveUsers returned an empty map, and every row on the page
	// rendered as a raw snowflake.
	resolveClient *http.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
	ttl   time.Duration
}

type cacheEntry struct {
	value   any
	expires time.Time
}

type BotGuild struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Icon        string `json:"icon,omitempty"`
	OwnerID     string `json:"owner_id,omitempty"`
	MemberCount int    `json:"member_count"`
	// CanViewAuditLog is only populated on the single-guild endpoint. False
	// means Discord will never tell the bot about moderation performed through
	// its own UI, which the moderation page explains rather than just showing
	// an empty table.
	CanViewAuditLog bool `json:"can_view_audit_log"`
}

type BotRole struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Color    int    `json:"color"`
	Position int    `json:"position"`
	Managed  bool   `json:"managed"`
}

type BotChannel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     int    `json:"type"`
	ParentID string `json:"parent_id,omitempty"`
	Position int    `json:"position"`
}

type BotUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Avatar      string `json:"avatar,omitempty"`
	// Member is false for someone who has left the guild. They still resolve to
	// a name and avatar; the flag only lets the UI mark them as a former member.
	Member bool `json:"member"`
}

// Discord channel type numbers the dashboard cares about.
const (
	ChannelTypeText         = 0
	ChannelTypeVoice        = 2
	ChannelTypeCategory     = 4
	ChannelTypeAnnouncement = 5
)

// IsText reports whether a channel can receive the bot's log and notification
// embeds.
func (c BotChannel) IsText() bool {
	return c.Type == ChannelTypeText || c.Type == ChannelTypeAnnouncement
}

func NewBotAPI(baseURL, secret string, ttl time.Duration) *BotAPI {
	return &BotAPI{
		baseURL:       strings.TrimRight(baseURL, "/"),
		secret:        secret,
		client:        &http.Client{Timeout: 5 * time.Second},
		resolveClient: &http.Client{Timeout: 30 * time.Second},
		cache:         make(map[string]cacheEntry),
		ttl:           ttl,
	}
}

// Configured reports whether a bot API is available at all. When it is not, the
// dashboard still serves every database-backed page.
func (b *BotAPI) Configured() bool {
	return b != nil && b.baseURL != "" && b.secret != ""
}

func (b *BotAPI) Guilds(ctx context.Context) ([]BotGuild, error) {
	return cached(b, ctx, "guilds", func() ([]BotGuild, error) {
		var out []BotGuild
		err := b.get(ctx, "/internal/guilds", &out)
		return out, err
	})
}

func (b *BotAPI) Guild(ctx context.Context, guildID snowflake.ID) (BotGuild, error) {
	return cached(b, ctx, "guild:"+guildID.String(), func() (BotGuild, error) {
		var out BotGuild
		err := b.get(ctx, "/internal/guilds/"+guildID.String(), &out)
		return out, err
	})
}

func (b *BotAPI) Roles(ctx context.Context, guildID snowflake.ID) ([]BotRole, error) {
	return cached(b, ctx, "roles:"+guildID.String(), func() ([]BotRole, error) {
		var out []BotRole
		err := b.get(ctx, "/internal/guilds/"+guildID.String()+"/roles", &out)
		return out, err
	})
}

func (b *BotAPI) Channels(ctx context.Context, guildID snowflake.ID) ([]BotChannel, error) {
	return cached(b, ctx, "channels:"+guildID.String(), func() ([]BotChannel, error) {
		var out []BotChannel
		err := b.get(ctx, "/internal/guilds/"+guildID.String()+"/channels", &out)
		return out, err
	})
}

// ResolveUsers turns snowflakes from modlogs and join/leave rows into names.
//
// It is a batch call because a single page of 50 modlog rows references up to
// 100 distinct users, and one request each would rate-limit the bot on first
// paint. IDs that cannot be resolved are simply absent from the map: a member
// who has since left is the normal case, not an error.
func (b *BotAPI) ResolveUsers(ctx context.Context, guildID snowflake.ID, ids []string) map[string]BotUser {
	out := make(map[string]BotUser)
	if !b.Configured() || len(ids) == 0 {
		return out
	}

	// Serve what is already known and ask only for the rest. Paging through a
	// moderation log asks about the same handful of moderators on every page,
	// and the same members recur across pages of a join/leave log.
	var missing []string
	b.mu.Lock()
	now := time.Now()
	for _, id := range ids {
		if entry, ok := b.cache[userCacheKey(guildID, id)]; ok && now.Before(entry.expires) {
			if user, ok := entry.value.(BotUser); ok {
				out[id] = user
				continue
			}
		}
		missing = append(missing, id)
	}
	b.mu.Unlock()

	if len(missing) == 0 {
		return out
	}

	body, err := json.Marshal(map[string]any{
		"guild_id": guildID.String(),
		"user_ids": missing,
	})
	if err != nil {
		return out
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.baseURL+"/internal/users/resolve", bytes.NewReader(body))
	if err != nil {
		return out
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", b.secret)

	resp, err := b.resolveClient.Do(req)
	if err != nil {
		slog.Warn("Resolving users failed; the page will show raw IDs",
			slog.Any("err", err), slog.Int("ids", len(missing)))
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("Resolving users returned an error status",
			slog.Int("status", resp.StatusCode), slog.Int("ids", len(missing)))
		return out
	}
	// A decode failure leaves the fetched set empty, which renders as raw IDs.
	// That is the intended degradation, so the error is dropped rather than
	// propagated.
	fetched := make(map[string]BotUser, len(missing))
	_ = json.NewDecoder(resp.Body).Decode(&fetched)

	b.mu.Lock()
	expires := time.Now().Add(b.ttl)
	for id, user := range fetched {
		out[id] = user
		b.cache[userCacheKey(guildID, id)] = cacheEntry{value: user, expires: expires}
	}
	b.mu.Unlock()

	// Unresolved IDs are deliberately not cached as misses here: the bot already
	// caches its own negative lookups, so a second round trip is the cheap half.
	return out
}

func userCacheKey(guildID snowflake.ID, userID string) string {
	return "user:" + guildID.String() + ":" + userID
}

func (b *BotAPI) get(ctx context.Context, path string, into any) error {
	if !b.Configured() {
		return ErrBotAPIUnavailable
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Internal-Token", b.secret)

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBotAPIUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrBotAPIUnavailable, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// cached memoises a fetch for the client's TTL. It is a free function rather
// than a method because Go does not allow type parameters on methods.
//
// Only successes are cached: a failure while the bot is restarting must not
// pin an error in place for the whole TTL.
func cached[T any](b *BotAPI, _ context.Context, key string, fetch func() (T, error)) (T, error) {
	b.mu.Lock()
	if entry, ok := b.cache[key]; ok && time.Now().Before(entry.expires) {
		if value, ok := entry.value.(T); ok {
			b.mu.Unlock()
			return value, nil
		}
	}
	b.mu.Unlock()

	value, err := fetch()
	if err != nil {
		return value, err
	}

	b.mu.Lock()
	b.cache[key] = cacheEntry{value: value, expires: time.Now().Add(b.ttl)}
	b.mu.Unlock()
	return value, nil
}

// ChannelLookup indexes channels by ID string for template rendering.
func ChannelLookup(channels []BotChannel) map[string]BotChannel {
	out := make(map[string]BotChannel, len(channels))
	for _, c := range channels {
		out[c.ID] = c
	}
	return out
}

// RoleLookup indexes roles by ID string for template rendering.
func RoleLookup(roles []BotRole) map[string]BotRole {
	out := make(map[string]BotRole, len(roles))
	for _, r := range roles {
		out[r.ID] = r
	}
	return out
}

// BotSingAlongRoll is what the bot reports after a re-roll.
type BotSingAlongRoll struct {
	Song string `json:"song"`
}

// ErrSingAlongRefused marks a re-roll the bot declined for a reason an admin can
// act on -- a missing permission, no channel set. The message is written for a
// person to read, so handlers show it rather than the degradation banner.
var ErrSingAlongRefused = errors.New("sing-along re-roll refused")

// RerollSingAlong asks the bot to replace a guild's sing-along round now.
//
// The bot answers as soon as the round is stored and live, and clears the channel
// afterwards in the background -- a wipe is minutes of paced Discord calls and this
// client gives up in five seconds. So a success here means "the new song is set",
// not "the channel is already empty".
func (b *BotAPI) RerollSingAlong(ctx context.Context, guildID snowflake.ID) (BotSingAlongRoll, error) {
	var out BotSingAlongRoll
	if !b.Configured() {
		return out, ErrBotAPIUnavailable
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.baseURL+"/internal/guilds/"+guildID.String()+"/singalong/reroll", nil)
	if err != nil {
		return out, fmt.Errorf("%w: %w", ErrBotAPIUnavailable, err)
	}
	req.Header.Set("X-Internal-Token", b.secret)

	resp, err := b.client.Do(req)
	if err != nil {
		return out, fmt.Errorf("%w: %w", ErrBotAPIUnavailable, err)
	}
	defer resp.Body.Close()

	// 422 is the bot saying no for a reason worth repeating verbatim; anything else
	// unexpected is a transport-shaped failure and degrades like the rest.
	if resp.StatusCode == http.StatusUnprocessableEntity {
		reason, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return out, fmt.Errorf("%w: %s", ErrSingAlongRefused, strings.TrimSpace(string(reason)))
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return out, fmt.Errorf("%w: status %d", ErrBotAPIUnavailable, resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("%w: %w", ErrBotAPIUnavailable, err)
	}
	return out, nil
}
