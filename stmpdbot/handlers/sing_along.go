package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/utils"
)

// singAlongTick is the check emoji held on a correct line, and the marker the
// listener removes again a few seconds later.
const SingAlongTick = "✅"

// singAlongRollAttempts is how many songs to try before giving up on a round.
//
// GetRandomSongWithLyricsEasy already excludes instrumentals and rows without
// lyrics, so a row that yields too few singable lines is rare -- a fragment stored
// as lyrics, or a track whose every line is shorter than the length floor. Retrying
// a handful of times turns that from a lost day into a different song.
const singAlongRollAttempts = 5

// singAlongMinRoundLines is the shortest song worth playing: one line for the bot to
// open with and one for somebody to answer.
const singAlongMinRoundLines = 2

// singAlongWipePages caps how many pages of 100 messages one wipe will delete.
//
// In steady state a channel holds a single day of chat and the cap is never
// approached. It matters exactly once, the first time the feature is pointed at a
// channel with years of history in it, where the alternative is a goroutine deleting
// messages one at a time for an hour.
const singAlongWipePages = 10

// ErrSingAlongNotConfigured is returned when a re-roll is asked for in a guild that
// has no sing-along channel set.
var ErrSingAlongNotConfigured = errors.New("no sing-along channel is configured for this server")

// singAlongPermissions is what the bot needs in the sing-along channel, and what
// each one is for.
//
// They are checked together and up front rather than discovered one failed API call
// at a time. A round that wipes the channel and then cannot post, or posts and then
// cannot delete a wrong answer, is worse than one that never starts and says why.
var singAlongPermissions = []struct {
	Flag discord.Permissions
	Name string
}{
	{discord.PermissionViewChannel, "View Channel"},
	{discord.PermissionSendMessages, "Send Messages"},
	{discord.PermissionManageMessages, "Manage Messages"},
	{discord.PermissionAddReactions, "Add Reactions"},
	{discord.PermissionReadMessageHistory, "Read Message History"},
	{discord.PermissionManageChannels, "Manage Channels"},
}

// RunSingAlong drops one Martin Garrix lyric a day into each configured channel and
// wipes the channel clean before the next one.
//
// Like the anniversaries this is not driven by a remote source changing; what it
// waits for is each guild's own configured hour, so the ticker is only a poll and
// the schedule itself lives in the guilds table.
func RunSingAlong(b *stmpdbot.STMPDBot, ticker *time.Ticker) {
	for ; ; <-ticker.C {
		runSingAlongCycle(b)
	}
}

func runSingAlongCycle(b *stmpdbot.STMPDBot) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	guilds, err := b.Queries.GetSingAlongGuilds(ctx)
	if err != nil {
		slog.Error("Failed to load sing-along guild configs", slog.Any("err", err))
		utils.RecordSourceFailure(utils.SourceSingAlong, err)
		return
	}

	// A per-guild problem -- a missing permission, an unresolvable zone -- is not a
	// failure of the source. utils/sourcehealth.go is explicit that a schedule-driven
	// source counts a cycle as successful whenever the configs could be read; doing
	// otherwise would let one misconfigured guild report the whole feature degraded.
	utils.RecordSourceSuccess(utils.SourceSingAlong)

	active := make([]snowflake.ID, 0, len(guilds))
	for _, guild := range guilds {
		active = append(active, snowflake.ID(guild.SingAlongChannel.Int64))

		if err := runSingAlongForGuild(ctx, b, guild); err != nil {
			slog.Error("Failed to run sing-along for guild",
				slog.Int64("guild_id", guild.GuildID),
				slog.Any("err", err))
		}
	}

	// Rebuilt against what is configured now, not merged into what was configured
	// before. A channel cleared in the dashboard would otherwise go on deleting
	// members' messages until the next restart.
	b.SingAlong.Retain(active)
}

func runSingAlongForGuild(ctx context.Context, b *stmpdbot.STMPDBot, guild db.GetSingAlongGuildsRow) error {
	guildID := snowflake.ID(guild.GuildID)
	channelID := snowflake.ID(guild.SingAlongChannel.Int64)

	// Same reasoning as the anniversaries: an unresolvable zone is skipped rather
	// than defaulted, because falling back to UTC does not fail loudly, it just runs
	// at the wrong hour forever.
	loc, ok := utils.ValidateTimezone(guild.SingAlongTimezone)
	if !ok {
		return fmt.Errorf("unresolvable timezone %q", guild.SingAlongTimezone)
	}

	// Rebuild the in-memory round if this is the first cycle after a restart, so the
	// message listener can start scoring again without waiting for the next roll.
	if _, live := b.SingAlong.Round(channelID); !live {
		if err := restoreSingAlongRound(ctx, b, guildID, channelID); err != nil {
			return err
		}
	}

	// Slowmode is reconciled every cycle rather than written when the setting
	// changes. The dashboard runs in its own process with no Discord client, so this
	// is how an edit there reaches Discord at all -- and reconciling also repairs a
	// slowmode somebody changed by hand. When nothing has drifted it costs no API
	// call.
	if err := syncSlowmode(b, channelID, int(guild.SingAlongCooldownMinutes)); err != nil {
		slog.Warn("Failed to sync sing-along slowmode",
			slog.Int64("guild_id", guild.GuildID),
			slog.Any("err", err))
	}

	now := time.Now().In(loc)

	// The window runs from the configured hour to local midnight rather than firing
	// on the hour, so a bot that was down at 09:00 and came back at 14:00 still gives
	// the server its lyric. The claim below is what stops it repeating.
	if now.Hour() < int(guild.SingAlongHour) {
		return nil
	}

	localDate := singAlongLocalDate(now)

	// Cheap pre-filter, so a guild that has already sung today is not re-rolled and
	// re-claimed every five minutes until midnight.
	//
	// It tests equality and nothing more, deliberately. ClaimSingAlongDay tolerates
	// the stored date being LATER than today -- a guild moved westward has its local
	// date go backward by a day -- and a pre-filter of "stored is not before today"
	// would swallow exactly that case before the claim ever saw it.
	if round, err := b.Queries.GetSingAlongRound(ctx, guild.GuildID); err == nil {
		if round.LocalDate.Valid && round.LocalDate.Time.Equal(localDate.Time) {
			return nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("failed to read sing-along round: %w", err)
	}

	return startSingAlongRound(ctx, b, guildID, channelID, localDate)
}

// restoreSingAlongRound rebuilds one guild's in-memory round from the database.
func restoreSingAlongRound(ctx context.Context, b *stmpdbot.STMPDBot, guildID, channelID snowflake.ID) error {
	stored, err := b.Queries.GetSingAlongRound(ctx, int64(guildID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("failed to restore sing-along round: %w", err)
	}

	round := utils.SingAlongRound{
		GuildID:   guildID,
		ChannelID: channelID,
		Lines:     stored.LyricLines,
		Cursor:    int(stored.Cursor),
		Complete:  stored.CompletedAt.Valid,
	}

	// The song may have been merged away since the round started. The lines are
	// frozen on the row, so the round is still playable; only the completion
	// message's artwork and links are lost.
	if stored.SongID.Valid {
		song, err := b.Queries.GetSongByID(ctx, stored.SongID.Int64)
		if err != nil {
			slog.Warn("Sing-along round refers to a song that could not be loaded",
				slog.Int64("song_id", stored.SongID.Int64),
				slog.Any("err", err))
		} else {
			round.Song, round.HasSong = song, true
		}
	}

	b.SingAlong.Set(round)
	return nil
}

// RerollSingAlong replaces a guild's round with a fresh song now, whatever the
// schedule says, and returns the song it picked. It backs the dashboard's re-roll
// button and /config sing-along-reroll.
//
// The wipe and the opening message are deliberately left to a goroutine. Clearing a
// channel is minutes of paced REST calls, and the dashboard's HTTP client to the bot
// gives up after five seconds -- so a synchronous re-roll would report the bot
// unreachable while the re-roll was in fact working. What is returned here is the
// part that is genuinely done: the round is stored and live, and the channel is
// about to catch up with it.
func RerollSingAlong(b *stmpdbot.STMPDBot) func(context.Context, snowflake.ID) (db.Song, error) {
	return func(ctx context.Context, guildID snowflake.ID) (db.Song, error) {
		config, err := b.Queries.GetGuild(ctx, int64(guildID))
		if err != nil {
			return db.Song{}, fmt.Errorf("failed to read server configuration: %w", err)
		}
		if !config.SingAlongChannel.Valid {
			return db.Song{}, ErrSingAlongNotConfigured
		}

		loc, ok := utils.ValidateTimezone(config.SingAlongTimezone)
		if !ok {
			return db.Song{}, fmt.Errorf("unresolvable timezone %q", config.SingAlongTimezone)
		}

		channelID := snowflake.ID(config.SingAlongChannel.Int64)

		if missing := missingSingAlongPermissions(b, guildID, channelID); len(missing) > 0 {
			return db.Song{}, fmt.Errorf("the bot is missing %s in <#%d>",
				strings.Join(missing, ", "), channelID)
		}

		song, lines, err := rollSingAlongSong(ctx, b)
		if err != nil {
			return db.Song{}, err
		}

		if err := b.Queries.ReplaceSingAlongRound(ctx, db.ReplaceSingAlongRoundParams{
			GuildID:    int64(guildID),
			LocalDate:  singAlongLocalDate(time.Now().In(loc)),
			SongID:     pgtype.Int8{Int64: song.ID, Valid: true},
			LyricLines: lines,
		}); err != nil {
			return db.Song{}, fmt.Errorf("failed to store sing-along round: %w", err)
		}

		round := utils.SingAlongRound{
			GuildID:   guildID,
			ChannelID: channelID,
			Song:      song,
			HasSong:   true,
			Lines:     lines,
			Cursor:    1,
		}

		// Live immediately, before the wipe, because the caller is told the re-roll
		// is done and the round is what "done" means. openSingAlongChannel sets the
		// same value again once the channel is clear, which costs nothing.
		b.SingAlong.Set(round)

		go func() {
			if err := syncSlowmode(b, channelID, int(config.SingAlongCooldownMinutes)); err != nil {
				slog.Warn("Failed to sync slowmode on re-roll", slog.Any("err", err))
			}
			openSingAlongChannel(b, round)
		}()

		return song, nil
	}
}

// startSingAlongRound is the scheduled ritual: check the bot can do the job, pick a
// song, take the day, wipe the channel and open with the first line.
func startSingAlongRound(
	ctx context.Context,
	b *stmpdbot.STMPDBot,
	guildID, channelID snowflake.ID,
	localDate pgtype.Date,
) error {
	// Gated before the claim, not after. Claiming and then discovering the bot
	// cannot delete or post burns the guild's day for nothing; skipping leaves the
	// claim untaken, so fixing the permission self-heals on the next five minute
	// tick.
	if missing := missingSingAlongPermissions(b, guildID, channelID); len(missing) > 0 {
		return fmt.Errorf("the bot is missing %s in <#%d>",
			strings.Join(missing, ", "), channelID)
	}

	song, lines, err := rollSingAlongSong(ctx, b)
	if err != nil {
		return err
	}

	// Claimed before anything is wiped or sent, so two ticks -- or two bot instances
	// -- cannot both empty the channel. Losing the claim is not an error; it is
	// exactly what the claim is for.
	claimed, err := b.Queries.ClaimSingAlongDay(ctx, db.ClaimSingAlongDayParams{
		GuildID:    int64(guildID),
		LocalDate:  localDate,
		SongID:     pgtype.Int8{Int64: song.ID, Valid: true},
		LyricLines: lines,
	})
	if err != nil {
		return fmt.Errorf("failed to claim sing-along day: %w", err)
	}
	if claimed == 0 {
		return nil
	}

	// Off the cycle goroutine. The day is claimed and stored, so nothing below
	// depends on it finishing -- and a first wipe of a channel with years of history
	// in it would otherwise hold up every other guild's sing-along behind it.
	go openSingAlongChannel(b, utils.SingAlongRound{
		GuildID:   guildID,
		ChannelID: channelID,
		Song:      song,
		HasSong:   true,
		Lines:     lines,
		Cursor:    1,
	})
	return nil
}

// openSingAlongChannel clears the channel, makes the round live, and posts the day's
// lyric into it.
//
// The order is what matters. Going live only after the wipe means the messages being
// deleted are never judged against a song whose opening line has not been posted --
// which, if the wipe failed halfway, would have the bot deleting everything anybody
// said in reply to yesterday's lyric.
//
// The round is already claimed and stored by the time this runs, so a failure here
// is logged rather than returned: unwinding a claim another bot instance may already
// be acting on is worse than a channel that is a little untidy, and the next cycle
// reads the round back from the database anyway.
func openSingAlongChannel(b *stmpdbot.STMPDBot, round utils.SingAlongRound) {
	// A budget of its own rather than the caller's. The scheduler's cycle context is
	// sized for a pass over every guild, and a first wipe of a channel with years of
	// history in it -- the one case that falls back to deleting a message at a time
	// -- can outlast that on its own. Being cut short is survivable: the day is
	// already claimed, so the leftovers go with tomorrow's wipe rather than
	// triggering a second one today.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	deleted, skipped, err := wipeChannel(ctx, b, round.ChannelID)
	if err != nil {
		slog.Error("Failed to fully wipe the sing-along channel",
			slog.Int64("guild_id", int64(round.GuildID)),
			slog.Int("deleted", deleted),
			slog.Any("err", err))
	}
	// Only ever a backlog that predates the feature: once a channel has been through
	// one daily cycle nothing in it is old enough to be skipped.
	if skipped > 0 {
		slog.Warn("Left messages too old for Discord to bulk delete",
			slog.Int64("guild_id", int64(round.GuildID)),
			slog.String("channel_id", round.ChannelID.String()),
			slog.Int("skipped", skipped))
	}

	b.SingAlong.Set(round)

	if _, err := b.Client.Rest.CreateMessage(round.ChannelID,
		discord.NewMessageCreate().WithEmbeds(singAlongOpeningEmbed(round.Lines[0]))); err != nil {
		slog.Error("Failed to post the sing-along lyric",
			slog.Int64("guild_id", int64(round.GuildID)),
			slog.Any("err", err))
		return
	}

	slog.Info("Started a sing-along round",
		slog.Int64("guild_id", int64(round.GuildID)),
		slog.Int64("song_id", round.Song.ID),
		slog.String("song", utils.SongHeading(round.Song.Artists, round.Song.Name, round.Song.MixName.String)),
		slog.Int("lines", len(round.Lines)),
		slog.Int("wiped", deleted),
		slog.Int("left", skipped))
}

// rollSingAlongSong picks a Martin Garrix song with enough singable lines.
//
// GetRandomSongWithLyricsEasy is reused rather than a new query: it is already
// "Martin Garrix, has lyrics, not an instrumental, not a collection, the canonical
// row, and not a remix by one of our own acts", which is exactly the Garrix-only
// catalogue this feature wants -- including the distinction between being credited
// on a record and it being theirs.
func rollSingAlongSong(ctx context.Context, b *stmpdbot.STMPDBot) (db.Song, []string, error) {
	for attempt := range singAlongRollAttempts {
		song, err := b.Queries.GetRandomSongWithLyricsEasy(ctx)
		if err != nil {
			return db.Song{}, nil, fmt.Errorf("failed to pick a sing-along song: %w", err)
		}

		lines := utils.LyricLines(song.Lyrics.String)
		if len(lines) >= singAlongMinRoundLines {
			return song, lines, nil
		}

		slog.Debug("Skipping a song with too few singable lines",
			slog.Int64("song_id", song.ID),
			slog.Int("lines", len(lines)),
			slog.Int("attempt", attempt+1))
	}

	return db.Song{}, nil, fmt.Errorf("no song with at least %d singable lines in %d attempts",
		singAlongMinRoundLines, singAlongRollAttempts)
}

// singAlongOpeningEmbed is the day's lyric.
//
// Deliberately without utils.GetSongButtonRows. Every other song message in the bot
// carries streaming links, and here they would hand out the answer to a game that
// has just started. The links go on the completion message instead.
func singAlongOpeningEmbed(first string) discord.Embed {
	return discord.NewEmbed().
		WithTitle("🎤 Sing along").
		WithDescription(fmt.Sprintf("> %s", first)).
		WithColor(utils.ColorSuccess).
		WithFooter("Reply with the next line of the song", "")
}

// SingAlongCompletionMessage celebrates a song sung all the way through, and is the
// one place a round gives the song away -- which is why the opener does not.
//
// A round whose song row was merged away mid-play still gets a message; it simply
// cannot name what was sung.
func SingAlongCompletionMessage(round utils.SingAlongRound) discord.MessageCreate {
	if !round.HasSong {
		return discord.NewMessageCreate().WithEmbeds(
			discord.NewEmbed().
				WithTitle("🎶 You sang the whole thing").
				WithColor(utils.ColorSuccess).
				WithFooter("A new lyric drops tomorrow", ""))
	}

	song := round.Song
	embed := discord.NewEmbed().
		WithTitle("🎶 You sang the whole thing").
		WithDescription(fmt.Sprintf("That was **%s**.",
			utils.SongHeading(song.Artists, song.Name, song.MixName.String))).
		WithColor(utils.ColorSuccess).
		WithFooter("A new lyric drops tomorrow", "")

	if song.ThumbnailUrl.Valid {
		embed = embed.WithThumbnail(song.ThumbnailUrl.String)
	}

	builder := discord.NewMessageCreate().WithEmbeds(embed)
	return builder.AddComponents(utils.GetSongButtonRows(song)...)
}

// singAlongLocalDate is the guild's own calendar day, stamped at midnight UTC.
//
// The stamp matters: pgtype.Date carries no zone, and a local-midnight time in a
// non-UTC zone is shifted to the previous or next day on the way into Postgres.
// Anniversaries do the same thing for the same reason.
func singAlongLocalDate(now time.Time) pgtype.Date {
	return pgtype.Date{
		Time:  time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC),
		Valid: true,
	}
}

// missingSingAlongPermissions names what the bot cannot do in the channel, in the
// words Discord's own UI uses, so the answer can be shown to an admin as-is.
func missingSingAlongPermissions(b *stmpdbot.STMPDBot, guildID, channelID snowflake.ID) []string {
	self, ok := b.Client.Caches.SelfMember(guildID)
	if !ok {
		return nil
	}
	channel, ok := b.Client.Caches.Channel(channelID)
	if !ok {
		return nil
	}

	held := b.Client.Caches.MemberPermissionsInChannel(channel, self)

	var missing []string
	for _, permission := range singAlongPermissions {
		if !held.Has(permission.Flag) {
			missing = append(missing, permission.Name)
		}
	}
	return missing
}

// syncSlowmode makes the channel's own rate limit match the configured cooldown.
//
// The cooldown is enforced by Discord rather than by the bot on purpose: a member who
// answers wrongly has already spent their window by the time the message is deleted,
// which is the behaviour asked for and needs no state of our own to track.
func syncSlowmode(b *stmpdbot.STMPDBot, channelID snowflake.ID, minutes int) error {
	channel, ok := b.Client.Caches.GuildTextChannel(channelID)
	if !ok {
		// Not cached, or not a text channel. Either way there is nothing to compare
		// against, and blindly PATCHing every cycle is worse than doing nothing.
		return nil
	}

	want := minutes * 60
	if channel.RateLimitPerUser() == want {
		return nil
	}

	if _, err := b.Client.Rest.UpdateChannel(channelID,
		discord.GuildTextChannelUpdate{RateLimitPerUser: &want}); err != nil {
		return fmt.Errorf("failed to set slowmode on <#%d>: %w", channelID, err)
	}

	slog.Info("Set sing-along slowmode",
		slog.String("channel_id", channelID.String()),
		slog.Int("seconds", want))
	return nil
}

// wipeChannel empties the channel and reports what it removed and what it could not.
//
// Only messages inside Discord's two-week bulk window are touched. Older ones are
// counted and left, because there is no bulk endpoint for them: clearing them means
// one heavily rate-limited call per message, which took over a minute per hundred,
// looked broken to everyone watching the channel, and still hit the page cap without
// finishing.
//
// That costs nothing in the steady state this feature actually runs in. The channel
// is emptied every day, so nothing in it is ever more than a day old and "everything
// bulk-deletable" and "everything" are the same set. What survives is only a backlog
// that predates the feature being pointed at the channel.
func wipeChannel(ctx context.Context, b *stmpdbot.STMPDBot, channelID snowflake.ID) (deleted, skipped int, err error) {
	for page := 0; page < singAlongWipePages; page++ {
		if err := ctx.Err(); err != nil {
			return deleted, skipped, err
		}

		// Always the newest hundred: the ones just deleted are gone, so this walks
		// the channel backwards without needing a cursor.
		messages, err := b.Client.Rest.GetMessages(channelID, 0, 0, 0, 100)
		if err != nil {
			return deleted, skipped, fmt.Errorf("failed to read messages to wipe: %w", err)
		}
		if len(messages) == 0 {
			return deleted, skipped, nil
		}

		fresh, stale := splitOnBulkDeleteAge(messages, time.Now())

		// Everything still visible is beyond the bulk window, so no further page can
		// contain anything this is willing to delete.
		if len(fresh) == 0 {
			return deleted, skipped + len(stale), nil
		}

		gone, err := bulkDelete(b, channelID, fresh)
		deleted += gone
		if err != nil {
			return deleted, skipped, err
		}
		// Nothing went, despite messages being in range -- a permission lost
		// mid-wipe, or a message Discord will not bulk delete. Re-reading the same
		// page until the cap only wastes the rate limit.
		if gone == 0 {
			return deleted, skipped, nil
		}
	}

	slog.Warn("Stopped wiping the sing-along channel at the page cap",
		slog.String("channel_id", channelID.String()),
		slog.Int("deleted", deleted))
	return deleted, skipped, nil
}

// bulkDeleteMaxAge is Discord's limit on what its bulk endpoint will accept. The
// margin is because the boundary is judged server-side against a clock that is not
// ours, and a message a second too old fails the whole batch.
const bulkDeleteMaxAge = 14*24*time.Hour - time.Hour

// splitOnBulkDeleteAge divides a page into what can be deleted in one call and what
// cannot be deleted quickly at all. A message's age comes from its snowflake, so this
// costs nothing and needs no extra field from the API.
func splitOnBulkDeleteAge(messages []discord.Message, now time.Time) (fresh, stale []snowflake.ID) {
	for _, message := range messages {
		if now.Sub(message.ID.Time()) < bulkDeleteMaxAge {
			fresh = append(fresh, message.ID)
		} else {
			stale = append(stale, message.ID)
		}
	}
	return fresh, stale
}

// bulkDelete removes a batch in one call, falling back only for the size Discord's
// bulk endpoint will not take.
func bulkDelete(b *stmpdbot.STMPDBot, channelID snowflake.ID, ids []snowflake.ID) (int, error) {
	switch len(ids) {
	case 0:
		return 0, nil
	case 1:
		// The endpoint takes two to a hundred, and disgo passes the slice straight
		// through, so the single-message case is ours to handle.
		if err := b.Client.Rest.DeleteMessage(channelID, ids[0]); err != nil {
			return 0, fmt.Errorf("failed to delete message: %w", err)
		}
		return 1, nil
	}

	if err := b.Client.Rest.BulkDeleteMessages(channelID, ids); err != nil {
		// Kept as a safety net for the boundary: the split above uses our clock, and
		// Discord judges the age with its own.
		var restErr *rest.Error
		if errors.As(err, &restErr) && restErr.Code == rest.JSONErrorCodeMessageTooOldToBulkDelete {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to bulk delete messages: %w", err)
	}
	return len(ids), nil
}
