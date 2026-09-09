package stmpdbot

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/milindmadhukar/STMPDBot/utils"
)

// The internal API lets the dashboard render role and channel pickers, guild
// names and member counts without ever holding the bot token. Everything here
// is served from the disgo cache first and falls back to REST on a miss, which
// is the same cache-first rule the rest of the bot follows.
//
// It listens on its own port rather than sharing the health mux: /health is
// deliberately unauthenticated and is hit by the container HEALTHCHECK, so
// keeping the two on separate listeners means publishing one to the host can
// never accidentally publish the other.

type internalGuild struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Icon        string `json:"icon,omitempty"`
	OwnerID     string `json:"owner_id,omitempty"`
	MemberCount int    `json:"member_count"`
	// CanViewAuditLog reports whether the BOT holds View Audit Log here.
	// Without it Discord never sends GUILD_AUDIT_LOG_ENTRY_CREATE, so
	// moderation done through Discord's UI is invisible and the dashboard's
	// moderation page would otherwise look empty for no stated reason.
	CanViewAuditLog bool `json:"can_view_audit_log"`
}

type internalRole struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Color    int    `json:"color"`
	Position int    `json:"position"`
	Managed  bool   `json:"managed"`
}

type internalChannel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     int    `json:"type"`
	ParentID string `json:"parent_id,omitempty"`
	Position int    `json:"position"`
}

type internalUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Avatar      string `json:"avatar,omitempty"`
	// Member is false for someone who has since left the guild. They still
	// resolve to a name and avatar via /users/{id}; the flag is only so the UI
	// can say "former member" rather than implying they are still here.
	Member bool `json:"member"`
}

type resolveRequest struct {
	GuildID string   `json:"guild_id"`
	UserIDs []string `json:"user_ids"`
}

// maxResolveIDs bounds a single resolve request. Every uncached ID costs one
// REST call, so an unbounded list is a way to make the bot rate-limit itself.
const maxResolveIDs = 100

const (
	// resolveTTL is how long a successful lookup is trusted. Display names and
	// avatars change rarely, and a stale one for a few minutes is harmless.
	resolveTTL = 15 * time.Minute
	// resolveMissTTL is shorter so somebody who rejoins shows up quickly.
	resolveMissTTL = 5 * time.Minute
)

// resolvedUser memoises one lookup, including the negative result.
type resolvedUser struct {
	user    internalUser
	found   bool
	expires time.Time
}

// StartInternalAPI serves live guild data to the dashboard on its own listener.
//
// It is a no-op when no secret is configured: an unauthenticated endpoint
// listing every guild the bot is in is worse than no endpoint at all, so a
// missing secret disables the server rather than opening it.
func (b *STMPDBot) StartInternalAPI() {
	if b.Cfg.Internal.Secret == "" {
		slog.Info("Internal API disabled: no secret configured")
		return
	}

	addr := b.Cfg.Internal.Address
	if addr == "" {
		addr = ":8082"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/guilds", b.handleInternalGuilds)
	mux.HandleFunc("GET /internal/guilds/{guildID}", b.handleInternalGuild)
	mux.HandleFunc("GET /internal/guilds/{guildID}/roles", b.handleInternalRoles)
	mux.HandleFunc("GET /internal/guilds/{guildID}/channels", b.handleInternalChannels)
	mux.HandleFunc("POST /internal/users/resolve", b.handleInternalResolve)
	mux.HandleFunc("POST /internal/guilds/{guildID}/singalong/reroll", b.handleInternalSingAlongReroll)

	srv := &http.Server{
		Addr:              addr,
		Handler:           b.internalAuth(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("Internal API listening", slog.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Internal API stopped", slog.Any("err", err))
		}
	}()
}

// internalAuth compares the presented token in constant time. A plain ==
// leaks the length of the shared prefix to anything that can time the response.
func (b *STMPDBot) internalAuth(next http.Handler) http.Handler {
	want := []byte(b.Cfg.Internal.Secret)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("X-Internal-Token"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (b *STMPDBot) handleInternalGuilds(w http.ResponseWriter, r *http.Request) {
	guilds := make([]internalGuild, 0)
	for g := range b.Client.Caches.Guilds() {
		guilds = append(guilds, internalGuild{
			ID:          g.ID.String(),
			Name:        g.Name,
			Icon:        derefString(g.Icon),
			OwnerID:     g.OwnerID.String(),
			MemberCount: g.MemberCount,
		})
	}
	sort.Slice(guilds, func(i, j int) bool { return guilds[i].Name < guilds[j].Name })
	writeInternalJSON(w, guilds)
}

func (b *STMPDBot) handleInternalGuild(w http.ResponseWriter, r *http.Request) {
	guildID, ok := parseGuildID(w, r)
	if !ok {
		return
	}

	g, found := b.Client.Caches.Guild(guildID)
	if !found {
		// A guild the bot is not in is a 404, not an error: the dashboard
		// uses this to decide whether to offer an invite link.
		http.NotFound(w, r)
		return
	}

	writeInternalJSON(w, internalGuild{
		ID:              g.ID.String(),
		Name:            g.Name,
		Icon:            derefString(g.Icon),
		OwnerID:         g.OwnerID.String(),
		MemberCount:     g.MemberCount,
		CanViewAuditLog: b.canViewAuditLog(guildID),
	})
}

// canViewAuditLog reports whether the bot itself can read this guild's audit
// log, which is what gates delivery of GUILD_AUDIT_LOG_ENTRY_CREATE.
//
// Computed from the member cache, which is only accurate because FlagRoles is
// enabled -- MemberPermissions needs the role objects to resolve anything.
func (b *STMPDBot) canViewAuditLog(guildID snowflake.ID) bool {
	self, ok := b.Client.Caches.Member(guildID, b.Client.ApplicationID)
	if !ok {
		// Unknown rather than false, but the caller only uses this to decide
		// whether to warn, and warning on a cache miss would cry wolf.
		return true
	}
	perms := b.Client.Caches.MemberPermissions(self)
	return perms.Has(discord.PermissionAdministrator) || perms.Has(discord.PermissionViewAuditLog)
}

func (b *STMPDBot) handleInternalRoles(w http.ResponseWriter, r *http.Request) {
	guildID, ok := parseGuildID(w, r)
	if !ok {
		return
	}

	roles := make([]internalRole, 0)
	for role := range b.Client.Caches.Roles(guildID) {
		roles = append(roles, newInternalRole(role))
	}

	// The cache is only populated when cache.FlagRoles is enabled and the
	// guild has been received; fall back to REST so a cold start or a
	// misconfigured cache degrades to slow rather than to empty dropdowns.
	if len(roles) == 0 {
		fetched, err := b.Client.Rest.GetRoles(guildID)
		if err != nil {
			slog.Error("Failed to fetch roles for internal API",
				slog.String("guild_id", guildID.String()), slog.Any("err", err))
			http.Error(w, "failed to fetch roles", http.StatusBadGateway)
			return
		}
		for _, role := range fetched {
			roles = append(roles, newInternalRole(role))
		}
	}

	// Highest role first, matching how Discord itself presents the list.
	sort.Slice(roles, func(i, j int) bool { return roles[i].Position > roles[j].Position })
	writeInternalJSON(w, roles)
}

func (b *STMPDBot) handleInternalChannels(w http.ResponseWriter, r *http.Request) {
	guildID, ok := parseGuildID(w, r)
	if !ok {
		return
	}

	channels := make([]internalChannel, 0)
	// Caches.Channels() is global, not guild-scoped, so this has to filter.
	for ch := range b.Client.Caches.Channels() {
		if ch.GuildID() != guildID {
			continue
		}
		channels = append(channels, newInternalChannel(ch))
	}

	if len(channels) == 0 {
		fetched, err := b.Client.Rest.GetGuildChannels(guildID)
		if err != nil {
			slog.Error("Failed to fetch channels for internal API",
				slog.String("guild_id", guildID.String()), slog.Any("err", err))
			http.Error(w, "failed to fetch channels", http.StatusBadGateway)
			return
		}
		for _, ch := range fetched {
			channels = append(channels, newInternalChannel(ch))
		}
	}

	sort.Slice(channels, func(i, j int) bool {
		if channels[i].Position != channels[j].Position {
			return channels[i].Position < channels[j].Position
		}
		return channels[i].ID < channels[j].ID
	})
	writeInternalJSON(w, channels)
}

// handleInternalResolve turns the raw snowflakes stored in modlogs and
// join_leave_logs into names the dashboard can render.
//
// Most rows in a join/leave log belong to people who have since left, and
// GET /guilds/{guild}/members/{user} 404s for them. Resolving only current
// members therefore left those pages a wall of bare snowflakes, so this falls
// through to GET /users/{id}, which resolves any account that still exists.
//
// An ID that resolves nowhere (deleted account) is simply absent from the
// response; the dashboard renders the raw ID for it.
func (b *STMPDBot) handleInternalResolve(w http.ResponseWriter, r *http.Request) {
	var req resolveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	guildID, err := snowflake.Parse(req.GuildID)
	if err != nil {
		http.Error(w, "invalid guild id", http.StatusBadRequest)
		return
	}

	if len(req.UserIDs) > maxResolveIDs {
		req.UserIDs = req.UserIDs[:maxResolveIDs]
	}

	out := make(map[string]internalUser, len(req.UserIDs))
	for _, raw := range req.UserIDs {
		userID, err := snowflake.Parse(raw)
		if err != nil {
			continue
		}
		if _, done := out[raw]; done {
			continue
		}

		if resolved, ok := b.resolveUser(guildID, userID); ok {
			out[raw] = resolved
		}
	}

	writeInternalJSON(w, out)
}

// resolveUser looks a user up cache-first, then as a guild member, then as a
// plain user, memoising the outcome.
//
// The cache matters because paging through a moderation log asks about the same
// handful of moderators on every page, and each miss is a REST call. Negative
// results are cached too, on a shorter TTL: without that, a log full of deleted
// accounts would re-request every one of them on every page load.
func (b *STMPDBot) resolveUser(guildID, userID snowflake.ID) (internalUser, bool) {
	key := guildID.String() + ":" + userID.String()

	b.resolveMu.Lock()
	if entry, ok := b.resolveCache[key]; ok && time.Now().Before(entry.expires) {
		b.resolveMu.Unlock()
		return entry.user, entry.found
	}
	b.resolveMu.Unlock()

	user, found := b.lookupUser(guildID, userID)

	ttl := resolveTTL
	if !found {
		ttl = resolveMissTTL
	}
	b.resolveMu.Lock()
	if b.resolveCache == nil {
		b.resolveCache = make(map[string]resolvedUser)
	}
	b.resolveCache[key] = resolvedUser{user: user, found: found, expires: time.Now().Add(ttl)}
	b.resolveMu.Unlock()

	return user, found
}

func (b *STMPDBot) lookupUser(guildID, userID snowflake.ID) (internalUser, bool) {
	// Current member: gives us the nickname and the per-guild avatar, which is
	// what people expect to see.
	if member, err := utils.GetMember(b.Client, guildID, userID); err == nil && member != nil {
		display := member.User.Username
		if member.Nick != nil && *member.Nick != "" {
			display = *member.Nick
		} else if member.User.GlobalName != nil && *member.User.GlobalName != "" {
			display = *member.User.GlobalName
		}
		return internalUser{
			ID:          userID.String(),
			Username:    member.User.Username,
			DisplayName: display,
			Avatar:      derefString(member.User.Avatar),
			Member:      true,
		}, true
	}

	// Former member. /users/{id} works for any account regardless of whether it
	// shares a guild with the bot, which is the whole point of this fallback.
	user, err := b.Client.Rest.GetUser(userID)
	if err != nil || user == nil {
		return internalUser{}, false
	}

	display := user.Username
	if user.GlobalName != nil && *user.GlobalName != "" {
		display = *user.GlobalName
	}
	return internalUser{
		ID:          userID.String(),
		Username:    user.Username,
		DisplayName: display,
		Avatar:      derefString(user.Avatar),
	}, true
}

func newInternalRole(role discord.Role) internalRole {
	return internalRole{
		ID:       role.ID.String(),
		Name:     role.Name,
		Color:    role.Color,
		Position: role.Position,
		Managed:  role.Managed,
	}
}

func newInternalChannel(ch discord.GuildChannel) internalChannel {
	c := internalChannel{
		ID:       ch.ID().String(),
		Name:     ch.Name(),
		Type:     int(ch.Type()),
		Position: ch.Position(),
	}
	if parent := ch.ParentID(); parent != nil {
		c.ParentID = parent.String()
	}
	return c
}

// internalSingAlongRoll is what the dashboard shows after a re-roll.
type internalSingAlongRoll struct {
	Song string `json:"song"`
}

// handleInternalSingAlongReroll replaces a guild's sing-along round on demand.
//
// It answers 202, not 200: what is finished when this returns is the round itself --
// stored, live, and already scoring -- while clearing the channel and posting the
// new lyric goes on in the background. The dashboard's client to this API gives up
// after five seconds and a wipe is minutes of paced REST calls, so doing the whole
// thing synchronously would report the bot unreachable while it was working.
func (b *STMPDBot) handleInternalSingAlongReroll(w http.ResponseWriter, r *http.Request) {
	guildID, ok := parseGuildID(w, r)
	if !ok {
		return
	}

	// Wired up in main.go once the gateway is ready. Before that the caches the
	// permission check reads are empty, so "not yet" is the honest answer.
	if b.SingAlongReroll == nil {
		http.Error(w, "the bot is still starting up", http.StatusServiceUnavailable)
		return
	}

	song, err := b.SingAlongReroll(r.Context(), guildID)
	if err != nil {
		// The message is written for an admin to read -- a missing permission names
		// the permission -- so it is passed through rather than swallowed.
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	// Headers before the status line, or neither of them reaches the client.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)

	if err := json.NewEncoder(w).Encode(internalSingAlongRoll{
		Song: utils.SongHeading(song.Artists, song.Name, song.MixName.String),
	}); err != nil {
		slog.Error("Failed to write sing-along re-roll response", slog.Any("err", err))
	}
}

func parseGuildID(w http.ResponseWriter, r *http.Request) (snowflake.ID, bool) {
	guildID, err := snowflake.Parse(r.PathValue("guildID"))
	if err != nil {
		http.Error(w, "invalid guild id", http.StatusBadRequest)
		return 0, false
	}
	return guildID, true
}

func writeInternalJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("Failed to write internal API response", slog.Any("err", err))
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
