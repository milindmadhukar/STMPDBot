package handlers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/utils"
)

// TODO: Announce anniversary of the song?

// TODO: Find some way to add lyrics to all stmpd songs
// Then we can do a stmpd level difficulty quiz lmao

// TODO: Add a way to remove songs manually (say before release remove)
// Add a way to add songs manually and annouect??

// TODO: All sets kb, when asked AI can query and send link in chat?

// stmpdLookbackDays bounds what the periodic fetcher asks the dataset for.
//
// This is a time window rather than a count. The previous fetcher kept the five
// newest releases per cycle, which quietly discarded anything beyond that: the
// label published eight releases in the sixty days to 2026-08-21, so three of them
// were never eligible to be seen at all. Sixty days is far more than the label's
// release cadence and still a tiny query.
const stmpdLookbackDays = 60

// stmpdReleaseParams renders a dataset release as the columns both the insert and
// the update take.
type stmpdReleaseParams struct {
	Name              string
	Artists           string
	Version           pgtype.Text
	ReleaseDate       pgtype.Text
	Slug              pgtype.Text
	Thumbnail         pgtype.Text
	Spotify           pgtype.Text
	AppleMusic        pgtype.Text
	YouTube           pgtype.Text
	YouTubeMusic      pgtype.Text
	Deezer            pgtype.Text
	Tidal             pgtype.Text
	AmazonMusic       pgtype.Text
	BeatportURL       pgtype.Text
	BeatportReleaseID pgtype.Int4
}

func newStmpdReleaseParams(r utils.SanityRelease) stmpdReleaseParams {
	l := r.StreamingLinks
	return stmpdReleaseParams{
		// The song's name, not "Title (Version)". The rendition goes in mix_name, so
		// every row records it the same way and unique_release can tell two
		// renditions of the same song released on the same day apart.
		Name:              r.Title,
		Artists:           r.Artists,
		Version:           utils.Text(r.Version),
		ReleaseDate:       utils.Text(r.ReleaseDate),
		Slug:              utils.Text(r.Slug),
		Thumbnail:         utils.Text(r.Artwork()),
		Spotify:           utils.Text(utils.CleanLink(l.Spotify)),
		AppleMusic:        utils.Text(utils.CleanLink(l.AppleMusic)),
		YouTube:           utils.Text(utils.NormalizeYoutubeURL(l.YouTube)),
		YouTubeMusic:      utils.Text(utils.CleanLink(l.YouTubeMusic)),
		Deezer:            utils.Text(utils.CleanLink(l.Deezer)),
		Tidal:             utils.Text(utils.CleanLink(l.Tidal)),
		AmazonMusic:       utils.Text(utils.CleanLink(l.AmazonMusic)),
		BeatportURL:       utils.Text(utils.CleanLink(l.Beatport)),
		BeatportReleaseID: utils.BeatportReleaseID(l.Beatport),
	}
}

func (p stmpdReleaseParams) insert() db.InsertReleaseParams {
	return db.InsertReleaseParams{
		Name: p.Name, Artists: p.Artists, MixName: p.Version, ReleaseDate: p.ReleaseDate,
		StmpdSlug: p.Slug, ThumbnailUrl: p.Thumbnail,
		SpotifyUrl: p.Spotify, AppleMusicUrl: p.AppleMusic,
		YoutubeUrl: p.YouTube, YoutubeMusicUrl: p.YouTubeMusic,
		DeezerUrl: p.Deezer, TidalUrl: p.Tidal, AmazonMusicUrl: p.AmazonMusic,
		BeatportUrl: p.BeatportURL, BeatportReleaseID: p.BeatportReleaseID,
	}
}

func (p stmpdReleaseParams) update(id int64) db.UpdateSongWithStmpdReleaseParams {
	return db.UpdateSongWithStmpdReleaseParams{
		ID: id, StmpdSlug: p.Slug, ReleaseDate: p.ReleaseDate,
		Title: utils.Text(p.Name), MixName: p.Version,
		ThumbnailUrl: p.Thumbnail,
		SpotifyUrl:   p.Spotify, AppleMusicUrl: p.AppleMusic,
		YoutubeUrl: p.YouTube, YoutubeMusicUrl: p.YouTubeMusic,
		DeezerUrl: p.Deezer, TidalUrl: p.Tidal, AmazonMusicUrl: p.AmazonMusic,
		BeatportUrl: p.BeatportURL, BeatportReleaseID: p.BeatportReleaseID,
	}
}

// GetAllStmpdReleases keeps the songs table in step with the STMPD catalogue.
//
// Identity is resolved slug first. The slug is unique and stable across all 1015
// dataset releases, so a release the bot has already stored is recognised as such
// regardless of how its name, artists or date have since been rewritten -- which is
// what allows this fetcher to correct a stored release_date instead of tiptoeing
// around the (name, artists, release_date) uniqueness constraint.
func GetAllStmpdReleases(b *stmpdbot.STMPDBot, ticker *time.Ticker) {
	client := utils.NewSanityClient()

	for ; ; <-ticker.C {
		slog.Info("Running STMPD RCRDS releases fetcher")

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		newCount, linkedCount, skippedCount := runStmpdCycle(ctx, b, client)
		cancel()

		slog.Info("STMPD sync complete",
			slog.Int("new", newCount),
			slog.Int("linked", linkedCount),
			slog.Int("skipped", skippedCount))
	}
}

func runStmpdCycle(ctx context.Context, b *stmpdbot.STMPDBot, client *utils.SanityClient) (newCount, linkedCount, skippedCount int) {
	releases, err := client.FetchStmpdReleases(ctx, utils.SinceDaysAgo(stmpdLookbackDays))
	if err != nil {
		slog.Error("Failed to fetch STMPD releases", slog.Any("err", err))
		utils.RecordSourceFailure(utils.SourceSTMPD, err)
		return
	}

	if len(releases) == 0 {
		slog.Warn("STMPD dataset returned no releases in the lookback window")
		utils.RecordSourceFailure(utils.SourceSTMPD, errors.New("dataset returned no releases"))
		return
	}

	utils.RecordSourceSuccess(utils.SourceSTMPD)

	existingSongs, err := b.Queries.GetAllSongsForMatching(ctx)
	if err != nil {
		slog.Error("Failed to load existing songs for STMPD matching", slog.Any("err", err))
		return
	}

	// Built once for the whole cycle. Every tier but the last is a map lookup.
	index := utils.NewSongIndex(existingSongs)

	notifier := utils.NewBatchNotifier(b.Queries, b.Client.Rest, utils.NotificationTypeSTMPD)

	for _, release := range releases {
		params := newStmpdReleaseParams(release)

		// The slug resolves a release the bot already stores regardless of how its
		// name, artists or date have since been rewritten; the remaining tiers are
		// what let a beatport-sourced row be recognised and gain its links.
		if matched, tier := index.Lookup(release.Query()); matched != nil {
			update := params.update(matched.ID)
			// The catalogue is normally the authority for a song's name, but not when
			// it has the title and artists the wrong way round -- as it did for
			// "Break It Down" on 2026-11-13. Writing the name through would put
			// "Firebeatz & Skytech" back on a row beatport had already corrected, and
			// the update cannot fix the artists to match because it never writes them.
			if utils.RolesSwapped(release.Title, release.Artists, matched.Name, matched.Artists) {
				update.Title = pgtype.Text{}
				slog.Warn("STMPD release has its title and artists swapped; keeping the stored name",
					slog.String("slug", release.Slug),
					slog.String("title", release.Title), slog.String("artists", release.Artists),
					slog.Int64("song_id", matched.ID))
			}
			rows, err := b.Queries.UpdateSongWithStmpdRelease(ctx, update)
			if err == nil {
				index.Claim(matched, release.Slug)
			}
			switch {
			case err != nil:
				slog.Error("Failed to apply STMPD release to existing song",
					slog.String("name", params.Name),
					slog.String("tier", string(tier)),
					slog.Any("err", err))
			case rows > 0:
				linkedCount++
				slog.Info("Applied STMPD release to existing song",
					slog.String("name", params.Name),
					slog.String("artists", params.Artists),
					slog.String("tier", string(tier)),
					slog.Int64("song_id", matched.ID))

				// The row may have just been promoted: someone added the track
				// because they heard it played, and it has now actually come out.
				// UpdateSongWithStmpdRelease clears announced_at in that case, so
				// re-reading tells us whether this is news.
				if updated, err := b.Queries.GetSongByID(ctx, matched.ID); err == nil {
					// The catalogue is the authority for a song's name, so this
					// update may have replaced it -- which invalidates the answerable
					// form derived from it. The query clears normalized_name when
					// that happens; this fills it back in before anything reads it.
					setNormalizedName(ctx, b, updated)
					announceSong(ctx, b, notifier, updated, params.Thumbnail.String)
				}
			default:
				skippedCount++
			}
			continue
		}

		// Genuinely new.
		song, err := b.Queries.InsertRelease(ctx, params.insert())
		if err != nil {
			// A row already holds this (name, artists, release_date). That is a
			// normal outcome, not a fault -- logging it at ERROR produced 1539 of
			// the error lines in the production log.
			if db.ErrorCode(err) == db.UniqueViolation {
				slog.Debug("STMPD release already stored",
					slog.String("name", params.Name), slog.String("artists", params.Artists))
				skippedCount++
				continue
			}
			slog.Error("Failed to insert release for "+params.Name, slog.Any("err", err))
			continue
		}

		newCount++
		finaliseNewSong(ctx, b, song, index)
		index.Append(db.GetAllSongsForMatchingRow{
			ID: song.ID, Name: song.Name, Artists: song.Artists, Source: song.Source,
			StmpdSlug: song.StmpdSlug, SpotifyUrl: song.SpotifyUrl,
			BeatportReleaseID: song.BeatportReleaseID, MixName: song.MixName,
			ReleaseDate: song.ReleaseDate,
		})

		announceSong(ctx, b, notifier, song, params.Thumbnail.String)
	}

	if err := notifier.Send(); err != nil {
		slog.Error("Failed to send batched STMPD notifications", slog.Any("err", err))
	}

	return newCount, linkedCount, skippedCount
}

// announceSong posts a song to the release channel if it has never been announced and
// is actually recent, and stamps the watermark either way.
//
// Two independent locks. announced_at was stamped on every row that existed before
// the watermark was introduced, so nothing already in the catalogue can be replayed;
// the recency window means even an unstamped row cannot push an old release into the
// channel. Both have to pass.
//
// The one case where a row that already existed does announce is promotion: a track
// someone added because they heard it played, which has since been released. The
// update clears announced_at precisely so this can happen.
func announceSong(ctx context.Context, b *stmpdbot.STMPDBot, notifier *utils.BatchNotifier, song db.Song, thumbnail string) {
	if song.AnnouncedAt.Valid || !isRecentRelease(song.ReleaseDate) {
		// Stamp it so that NULL keeps meaning "still pending" rather than
		// accumulating rows that were considered and deliberately passed over.
		if !song.AnnouncedAt.Valid {
			if err := b.Queries.MarkSongAnnounced(ctx, song.ID); err != nil {
				slog.Error("Failed to mark song announced",
					slog.Int64("song_id", song.ID), slog.Any("err", err))
			}
		}
		return
	}

	if thumbnail == "" {
		thumbnail = song.ThumbnailUrl.String
	}

	embed := discord.NewEmbed().
		WithTitle(utils.SongHeading(song.Artists, song.Name, song.MixName.String)).
		WithImage(thumbnail).
		WithFooter("Released "+song.ReleaseDate.String, "")

	notifier.AddItem(utils.NotificationItem{
		Embed:      &embed,
		Components: utils.GetSongButtonRows(song),
		// Remember where this lands. Most of this song's streaming links do not exist
		// yet -- they arrive from later STMPD cycles, the hourly enrichment and the
		// backfill scripts -- and without the message id the announcement would keep
		// today's buttons forever.
		SongID:        song.ID,
		LinkSignature: utils.SongLinkSignature(song),
	})

	// Stamped when the item joins the batch rather than after the batch is sent. A
	// failed send loses one announcement; not stamping would risk replaying the whole
	// batch next cycle, and quiet is the safer failure.
	if err := b.Queries.MarkSongAnnounced(ctx, song.ID); err != nil {
		slog.Error("Failed to mark song announced",
			slog.Int64("song_id", song.ID), slog.Any("err", err))
	}
}
