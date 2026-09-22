package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/utils"
)

// maxSpotlights caps how many songs get their own dedicated message in a day.
//
// A milestone deserves more than a line in a list, but the catalogue has days that
// are not what they look like: thirteen songs share release_date 2017-12-01, which
// is when the STMPD back catalogue was imported rather than when any of them came
// out. Without a cap that one day would fire thirteen separate messages.
const maxSpotlights = 3

// discordMaxEmbedDescription is Discord's limit on an embed description. Going over
// rejects the whole message rather than truncating it.
const discordMaxEmbedDescription = 4096

// anniversaryNoonUTC turns a release date into the timestamp the message embeds.
//
// Discord renders <t:...:D> in each viewer's own timezone, so a date anchored at
// midnight UTC shows up as the previous day for everyone west of Greenwich. Noon is
// the only anchor that lands on the right calendar day for every offset from UTC-12
// through UTC+11, which is every timezone with a meaningful population.
func anniversaryNoonUTC(d time.Time) int64 {
	return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, time.UTC).Unix()
}

// anniversaryMonthDays is the set of 'MM-DD' keys to announce on a given local date.
//
// It is normally just today. The exception is 1 March in a non-leap year, which also
// carries 29 February: a leap-day release would otherwise go unmentioned in three
// years out of four, which is the opposite of what an anniversary feature is for.
func anniversaryMonthDays(today time.Time) []string {
	keys := []string{today.Format("01-02")}

	isLeapYear := func(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }
	if today.Month() == time.March && today.Day() == 1 && !isLeapYear(today.Year()) {
		keys = append(keys, "02-29")
	}

	return keys
}

// anniversaryLine is the one-line summary of a song's anniversary.
//
// The year count is written out as literal text rather than left to <t:...:R>.
// Discord's relative timestamp rounds, and a post that goes out at 09:00 local is a
// few hours short of the exact anniversary of a noon-UTC anchor, which is enough for
// it to render "2 years ago" on a song's third birthday.
//
// So <t:...:R> is not printed at all. It used to sit beside the literal count as a
// flourish, which made every line say the number twice ("**9 years** ago today,
// September 22, 2017 (9 years ago)") and, on exactly the boundary this function
// exists to get right, say it twice differently -- "**3 years** ago today, September
// 22, 2023 (2 years ago)". A stamp documented as unreliable has no business rendering
// next to the one that is correct. Only the absolute date goes out, and that is exact.
func anniversaryLine(song db.Song, years int32, ts int64) string {
	title := fmt.Sprintf("**%s - %s**", song.Artists, song.Name)

	switch years {
	case 1:
		return fmt.Sprintf("🎂 %s turns **1 year old** today! Released <t:%d:D>", title, ts)
	case 5:
		return fmt.Sprintf("🎉 %s - **5 YEARS** today! Released <t:%d:D>", title, ts)
	case 10:
		return fmt.Sprintf("🏆 %s - **A DECADE** today! Released <t:%d:D>", title, ts)
	default:
		return fmt.Sprintf("🎵 %s - **%d years** ago today, <t:%d:D>", title, years, ts)
	}
}

// isMilestone reports whether an anniversary is worth its own message.
func isMilestone(years int32) bool {
	return years == 1 || years == 5 || years == 10
}

// spotlightHeading is the headline for a milestone's dedicated message.
func spotlightHeading(years int32) string {
	switch years {
	case 1:
		return "🎂 One year ago today"
	case 5:
		return "🎉 Five years ago today"
	case 10:
		return "🏆 Ten years ago today"
	default:
		return "🎵 On this day"
	}
}

// GetSongAnniversaries announces songs whose release date falls on today's month
// and day, once per guild per local day.
//
// Unlike the other fetchers this one is not driven by a remote source changing --
// the catalogue is already local. What it waits for is each guild's own morning, so
// the ticker is only a poll and the real schedule lives in the guilds table.
func GetSongAnniversaries(b *stmpdbot.STMPDBot, ticker *time.Ticker) {
	for ; ; <-ticker.C {
		runAnniversaryCycle(b)
	}
}

func runAnniversaryCycle(b *stmpdbot.STMPDBot) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	guilds, err := b.Queries.GetAnniversaryGuilds(ctx)
	if err != nil {
		slog.Error("Failed to load anniversary guild configs", slog.Any("err", err))
		utils.RecordSourceFailure(utils.SourceAnniversary, err)
		return
	}

	utils.RecordSourceSuccess(utils.SourceAnniversary)

	for _, guild := range guilds {
		if err := runAnniversaryForGuild(ctx, b, guild); err != nil {
			slog.Error("Failed to run anniversaries for guild",
				slog.Int64("guild_id", guild.GuildID),
				slog.Any("err", err))
		}
	}
}

func runAnniversaryForGuild(ctx context.Context, b *stmpdbot.STMPDBot, guild db.GetAnniversaryGuildsRow) error {
	// A timezone we cannot resolve is skipped rather than defaulted. Falling back to
	// UTC would not fail loudly, it would just post at the wrong hour forever.
	// ValidateTimezone rather than time.LoadLocation because a row edited by hand in
	// psql could say "Local", which LoadLocation accepts and binds to the log config's
	// zone -- the slash command can no longer write that, but the column predates it.
	loc, ok := utils.ValidateTimezone(guild.AnniversaryTimezone)
	if !ok {
		return fmt.Errorf("unresolvable timezone %q", guild.AnniversaryTimezone)
	}

	now := time.Now().In(loc)

	// The window is "from the configured hour until local midnight", not "at the
	// configured hour". A bot that was down at 09:00 and came back at 14:00 should
	// still deliver the day's post; the claim row below is what stops it repeating.
	if now.Hour() < int(guild.AnniversaryHour) {
		return nil
	}

	localDate := pgtype.Date{
		Time:  time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC),
		Valid: true,
	}

	posted, err := b.Queries.HasPostedAnniversaries(ctx, db.HasPostedAnniversariesParams{
		GuildID:   guild.GuildID,
		LocalDate: localDate,
	})
	if err != nil {
		return fmt.Errorf("failed to check anniversary claim: %w", err)
	}
	if posted {
		return nil
	}

	rows, err := b.Queries.GetSongAnniversaries(ctx, db.GetSongAnniversariesParams{
		Today:     localDate,
		MonthDays: anniversaryMonthDays(now),
	})
	if err != nil {
		return fmt.Errorf("failed to load anniversaries: %w", err)
	}

	// A day with nothing in it is still a day that has been handled. Claiming it
	// keeps the guild from being re-queried every five minutes until midnight.
	claimed, err := b.Queries.ClaimAnniversaryDay(ctx, db.ClaimAnniversaryDayParams{
		GuildID:   guild.GuildID,
		LocalDate: localDate,
		SongCount: int32(len(rows)),
	})
	if err != nil {
		return fmt.Errorf("failed to claim anniversary day: %w", err)
	}

	// Someone else took the day between the pre-filter and here. Not an error --
	// exactly what the claim exists to do.
	if claimed == 0 {
		return nil
	}

	if len(rows) == 0 {
		slog.Debug("No song anniversaries today",
			slog.Int64("guild_id", guild.GuildID),
			slog.String("local_date", now.Format(time.DateOnly)))
		return nil
	}

	slog.Info("Announcing song anniversaries",
		slog.Int64("guild_id", guild.GuildID),
		slog.String("local_date", now.Format(time.DateOnly)),
		slog.Int("count", len(rows)))

	sendAnniversaries(b, guild, rows, now)
	return nil
}

// PreviewAnniversaries renders what a guild's post would look like right now,
// without claiming the day or sending anything.
//
// This backs /config anniversary-preview. It exists because the two things most
// likely to be subtly wrong -- whether <t:...:D> lands on the right calendar day and
// whether <t:...:R> agrees with the year count written beside it -- can only really
// be judged by looking at a rendered message, and waiting until 9am to find out is a
// poor feedback loop.
func PreviewAnniversaries(ctx context.Context, b *stmpdbot.STMPDBot, timezone string) (string, []discord.Embed, error) {
	loc, ok := utils.ValidateTimezone(timezone)
	if !ok {
		return "", nil, fmt.Errorf("unresolvable timezone %q", timezone)
	}

	now := time.Now().In(loc)
	localDate := pgtype.Date{
		Time:  time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC),
		Valid: true,
	}

	rows, err := b.Queries.GetSongAnniversaries(ctx, db.GetSongAnniversariesParams{
		Today:     localDate,
		MonthDays: anniversaryMonthDays(now),
	})
	if err != nil {
		return "", nil, fmt.Errorf("failed to load anniversaries: %w", err)
	}

	if len(rows) == 0 {
		return fmt.Sprintf("No song anniversaries on %s. Nothing would be posted today.",
			now.Format("2 January")), nil, nil
	}

	embeds := []discord.Embed{buildSummaryEmbed(rows, now)}

	spotlights := 0
	for _, row := range rows {
		if spotlights >= maxSpotlights {
			break
		}
		if !isMilestone(row.YearsOld) {
			continue
		}
		if embed, ok := buildSpotlightEmbed(row); ok {
			embeds = append(embeds, embed)
			spotlights++
		}
	}

	have := "have"
	if len(rows) == 1 {
		have = "has"
	}

	return fmt.Sprintf("Preview for **%s** in `%s` - %s %s an anniversary today.",
		now.Format("2 January 2006"), timezone, pluralSongs(len(rows)), have), embeds, nil
}

// buildSummaryEmbed renders the whole day as one embed, one line per song.
//
// One message for the day rather than one per song: the catalogue averages three
// anniversaries a day but has days with sixteen, and sixteen separate messages is
// not an announcement, it is a flood.
func buildSummaryEmbed(rows []db.GetSongAnniversariesRow, today time.Time) discord.Embed {
	lines := make([]string, 0, len(rows))
	used := 0
	omitted := 0

	for _, row := range rows {
		released, err := time.Parse(time.DateOnly, row.Song.ReleaseDate.String)
		if err != nil {
			// The query only matches well-formed dates, so this is unreachable in
			// practice -- but a bad row should cost one line, not the whole post.
			slog.Warn("Skipping anniversary with unparseable release_date",
				slog.String("release_date", row.Song.ReleaseDate.String))
			continue
		}

		line := anniversaryLine(row.Song, row.YearsOld, anniversaryNoonUTC(released))

		// Leave room for the "and N more" tail rather than discovering the overflow
		// once the embed is already too long to send.
		if used+len(line)+1 > discordMaxEmbedDescription-64 {
			omitted++
			continue
		}

		lines = append(lines, line)
		used += len(line) + 1
	}

	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("...and **%d** more.", omitted))
	}

	embed := discord.NewEmbed().
		WithTitle(fmt.Sprintf("On This Day - %s", today.Format("2 January"))).
		WithDescription(strings.Join(lines, "\n")).
		WithColor(utils.ColorSuccess).
		WithFooter(fmt.Sprintf("%s in Martin Garrix history", pluralSongs(len(rows))), "")

	// The thumbnail comes from the oldest song of the day -- the rows are ordered by
	// release_date -- which is the one the day is most likely to be remembered for.
	for _, row := range rows {
		if row.Song.ThumbnailUrl.Valid && row.Song.ThumbnailUrl.String != "" {
			embed = embed.WithThumbnail(row.Song.ThumbnailUrl.String)
			break
		}
	}

	return embed
}

func pluralSongs(n int) string {
	if n == 1 {
		return "1 song"
	}
	return fmt.Sprintf("%d songs", n)
}

// buildSpotlightEmbed renders one milestone song as its own embed.
func buildSpotlightEmbed(row db.GetSongAnniversariesRow) (discord.Embed, bool) {
	released, err := time.Parse(time.DateOnly, row.Song.ReleaseDate.String)
	if err != nil {
		return discord.Embed{}, false
	}

	ts := anniversaryNoonUTC(released)

	embed := discord.NewEmbed().
		WithAuthor(spotlightHeading(row.YearsOld), "", "").
		WithTitle(utils.SongHeading(row.Song.Artists, row.Song.Name, row.Song.MixName.String)).
		WithDescription(fmt.Sprintf("Released <t:%d:D> - <t:%d:R>", ts, ts)).
		WithColor(utils.ColorSuccess)

	if row.Song.ThumbnailUrl.Valid && row.Song.ThumbnailUrl.String != "" {
		embed = embed.WithImage(row.Song.ThumbnailUrl.String)
	}

	return embed, true
}

// sendAnniversaries posts one guild's day.
//
// This deliberately does not go through utils.BatchNotifier. That type fans a single
// batch out to every configured guild at once, which is right for a feed that fires
// when a remote source changes; here each guild fires at its own local hour with its
// own claim row, and the summary embed's wording is per-guild too.
func sendAnniversaries(b *stmpdbot.STMPDBot, guild db.GetAnniversaryGuildsRow, rows []db.GetSongAnniversariesRow, today time.Time) {
	channelID := snowflake.ID(guild.AnniversaryNotificationsChannel.Int64)

	have := "have"
	if len(rows) == 1 {
		have = "has"
	}

	content := fmt.Sprintf("%s %s an anniversary today 🎂", pluralSongs(len(rows)), have)
	if guild.AnniversaryNotificationsRole.Valid {
		content = fmt.Sprintf("<@&%d>, %s", guild.AnniversaryNotificationsRole.Int64, content)
	}

	summary := buildSummaryEmbed(rows, today)

	if _, err := b.Client.Rest.CreateMessage(channelID,
		discord.NewMessageCreate().
			WithContent(content).
			WithEmbeds(summary)); err != nil {
		slog.Error("Failed to send anniversary summary",
			slog.Int64("guild_id", guild.GuildID),
			slog.Uint64("channel_id", uint64(channelID)),
			slog.Any("err", err))
		return
	}

	spotlights := 0
	for _, row := range rows {
		if spotlights >= maxSpotlights {
			break
		}
		if !isMilestone(row.YearsOld) {
			continue
		}

		embed, ok := buildSpotlightEmbed(row)
		if !ok {
			continue
		}

		time.Sleep(500 * time.Millisecond)

		// GetSongButtonRows returns nil for a song with no streaming links, so the
		// linkless case needs no guard here. AddComponents takes a value receiver, so
		// the result has to be assigned back or the rows are silently dropped.
		builder := discord.NewMessageCreate().
			WithEmbeds(embed).
			AddComponents(utils.GetSongButtonRows(row.Song)...)

		if _, err := b.Client.Rest.CreateMessage(channelID, builder); err != nil {
			slog.Error("Failed to send anniversary spotlight",
				slog.Int64("guild_id", guild.GuildID),
				slog.String("song", row.Song.Name),
				slog.Any("err", err))
			continue
		}

		spotlights++
	}
}
