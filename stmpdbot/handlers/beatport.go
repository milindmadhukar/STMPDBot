package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/utils"
)

// GetBeatportReleases periodically fetches new songs from the Beatport API
func GetBeatportReleases(b *stmpdbot.STMPDBot, ticker *time.Ticker) {
	// Credentials only change with a restart, so there is nothing to retry for.
	if b.Cfg.Bot.BeatportUsername == "" || b.Cfg.Bot.BeatportPassword == "" {
		slog.Warn("Beatport credentials not configured, skipping beatport releases fetcher")
		return
	}

	for ; ; <-ticker.C {
		slog.Info("Running Beatport releases fetcher")

		// Initialise lazily. NewBeatportClient performs network calls to obtain a
		// client ID and log in, so a transient failure while the bot was starting
		// must not disable the fetcher for the lifetime of the process.
		if b.BeatportClient == nil {
			if err := b.SetupBeatport(); err != nil {
				slog.Error("Failed to initialize beatport client, retrying next cycle", slog.Any("err", err))
				continue
			}
			if b.BeatportClient == nil {
				slog.Error("Beatport client unavailable after setup, retrying next cycle")
				continue
			}
		}

		// Authenticate once per cycle rather than letting each of the five source
		// fetches below do it on demand. A rejected credential used to surface as
		// five identical login attempts and five ERROR lines every 15 minutes; now
		// it costs one attempt, and the client's cooldown suppresses even that.
		if err := b.BeatportClient.EnsureAuthenticated(); err != nil {
			slog.Error("Beatport authentication unavailable, skipping this cycle", slog.Any("err", err))
			utils.RecordSourceFailure(utils.SourceBeatport, err)
			continue
		}

		maxTracks := b.Cfg.Bot.BeatportMaxTracks

		var allTracks []utils.BeatportTrack

		// Fetch from label
		if b.Cfg.Bot.BeatportLabelID != "" {
			slog.Info("Fetching Beatport tracks from label", slog.String("label_id", b.Cfg.Bot.BeatportLabelID))
			labelTracks, err := b.BeatportClient.GetAllLabelTracks(b.Cfg.Bot.BeatportLabelID, maxTracks)
			if err != nil {
				slog.Error("Failed to fetch beatport label tracks", slog.Any("err", err))
			} else {
				allTracks = append(allTracks, labelTracks...)
			}
		}

		// Fetch from artists
		for _, artistID := range b.Cfg.Bot.BeatportArtistIDs {
			slog.Info("Fetching Beatport tracks from artist", slog.String("artist_id", artistID))
			artistTracks, err := b.BeatportClient.GetAllArtistTracks(artistID, maxTracks)
			if err != nil {
				slog.Error("Failed to fetch beatport artist tracks",
					slog.String("artist_id", artistID), slog.Any("err", err))
				continue
			}
			allTracks = append(allTracks, artistTracks...)
		}

		// Deduplicate by beatport track ID
		trackMap := make(map[int]utils.BeatportTrack)
		for _, track := range allTracks {
			trackMap[track.ID] = track
		}

		slog.Info("Beatport total unique tracks fetched", slog.Int("count", len(trackMap)))

		// Zero tracks after a successful auth means every source call failed or the
		// catalogue shape changed. Either way it is not a healthy cycle, and saying
		// so is what turns four days of "sync complete new=0" into an alert.
		if len(trackMap) == 0 {
			utils.RecordSourceFailure(utils.SourceBeatport, errors.New("no tracks returned by any configured source"))
		} else {
			utils.RecordSourceSuccess(utils.SourceBeatport)
		}

		// Load existing songs for similarity matching
		existingSongs, err := b.Queries.GetAllSongsForMatching(context.Background())
		if err != nil {
			slog.Error("Failed to load existing songs for matching", slog.Any("err", err))
			continue
		}

		// Built once for the whole cycle. The previous matcher rebuilt nothing and
		// instead ran a Levenshtein comparison against all ~1400 rows for every one
		// of the ~190 tracks fetched, every 15 minutes.
		index := utils.NewSongIndex(existingSongs)

		notifier := utils.NewBatchNotifier(b.Queries, b.Client.Rest, utils.NotificationTypeSTMPD)

		newCount := 0
		updatedCount := 0
		skippedCount := 0

		for _, track := range trackMap {
			artistsStr := utils.FormatBeatportArtists(track.Artists)

			// Check if this beatport track already exists by beatport_id
			existingSong, err := b.Queries.GetSongByBeatportID(context.Background(), pgtype.Int4{
				Int32: int32(track.ID),
				Valid: true,
			})
			if err == nil { // Song exists with this beatport_id
				// The slug is stored before either skip below, because both of them
				// return and this is the only place the authoritative value is ever in
				// hand. A track page is /track/<slug>/<id>, so a row holding the id
				// without the slug has a Beatport button that 404s -- and a row marked
				// beatport_updated before slugs were captured would otherwise be
				// skipped here forever and never acquire one. Filling it on the way
				// past is what makes that class self-healing instead of a standing
				// import-beatport chore.
				fillBeatportSlug(b, existingSong, track.Slug)

				if existingSong.BeatportUpdated {
					skippedCount++
					continue
				}

				// Check if updating would cause a duplicate key conflict
				conflicts, _ := b.Queries.DoesSongExist(context.Background(), db.DoesSongExistParams{
					Name:        track.Name,
					Artists:     artistsStr,
					ReleaseDate: utils.Text(track.ReleaseDate),
				})
				if conflicts {
					// Another row already has this (name, artists, release_date) — just mark done
					_ = b.Queries.MarkBeatportUpdated(context.Background(), existingSong.ID)
					skippedCount++
					continue
				}

				rows, err := b.Queries.UpdateSongWithBeatportData(context.Background(), db.UpdateSongWithBeatportDataParams{
					ID:      existingSong.ID,
					Name:    track.Name,
					Artists: artistsStr,
					ThumbnailUrl: pgtype.Text{
						String: track.ThumbnailURL,
						Valid:  track.ThumbnailURL != "",
					},
					BeatportID: pgtype.Int4{
						Int32: int32(track.ID),
						Valid: true,
					},
					BeatportSlug: pgtype.Text{
						String: track.Slug,
						Valid:  track.Slug != "",
					},
					MixName: pgtype.Text{
						String: track.MixName,
						Valid:  track.MixName != "",
					},
					ReleaseDate: utils.Text(track.ReleaseDate),
					ReleaseName: pgtype.Text{
						String: track.Release.Name,
						Valid:  track.Release.Name != "",
					},
					Genre: pgtype.Text{
						String: track.Genre.Name,
						Valid:  track.Genre.Name != "",
					},
					SubGenre: pgtype.Text{
						String: track.SubGenre.Name,
						Valid:  track.SubGenre.Name != "",
					},
					Bpm: pgtype.Int4{
						Int32: int32(track.BPM),
						Valid: track.BPM > 0,
					},
					MusicalKey: pgtype.Text{
						String: track.Key.Name,
						Valid:  track.Key.Name != "",
					},
					LengthMs: pgtype.Int4{
						Int32: int32(track.LengthMs),
						Valid: track.LengthMs > 0,
					},
				})

				switch {
				case err != nil:
					slog.Error("Failed to update song with beatport data",
						slog.String("name", track.Name), slog.Any("err", err))
				case rows > 0:
					updatedCount++
				default:
					// The row already held exactly this data. Counting it as an
					// update is what made every cycle report updated=~73 forever.
					skippedCount++
				}
				continue
			}

			// Not found by beatport_id: resolve against everything else that could
			// identify this recording. Artists are compared as a set here, so
			// beatport's "Ed Sheeran, Martin Garrix" now finds the STMPD row filed
			// under "Martin Garrix & Ed Sheeran" -- the pairing the old whole-string
			// Levenshtein match could never make.
			matchedSong, matchTier := index.Lookup(utils.SongQuery{
				Title:   track.Name,
				MixName: track.MixName,
				Artists: artistsStr,
				BeatportID: pgtype.Int4{
					Int32: int32(track.ID),
					Valid: true,
				},
				ReleaseDate: track.ReleaseDate,
			})

			// The recording is already stored under a different beatport track id.
			// Beatport issues several ids per recording and songs.beatport_id holds
			// one, so there is nothing to write and nothing to insert.
			if matchTier == utils.MatchAlreadyRepresented {
				slog.Debug("Beatport track already represented under another id",
					slog.String("name", track.Name),
					slog.Int64("song_id", matchedSong.ID))
				skippedCount++
				continue
			}

			if matchedSong != nil {
				slog.Debug("Matched beatport track to existing song",
					slog.String("name", track.Name),
					slog.String("tier", string(matchTier)),
					slog.Int64("song_id", matchedSong.ID))
				// Check if updating would cause a duplicate key conflict
				conflicts, _ := b.Queries.DoesSongExist(context.Background(), db.DoesSongExistParams{
					Name:        track.Name,
					Artists:     artistsStr,
					ReleaseDate: utils.Text(track.ReleaseDate),
				})
				if conflicts {
					_ = b.Queries.MarkBeatportUpdated(context.Background(), matchedSong.ID)
					skippedCount++
					continue
				}

				// Similar song exists (from STMPD) — update it with beatport data
				rows, err := b.Queries.UpdateSongWithBeatportData(context.Background(), db.UpdateSongWithBeatportDataParams{
					ID:      matchedSong.ID,
					Name:    track.Name,
					Artists: artistsStr,
					ThumbnailUrl: pgtype.Text{
						String: track.ThumbnailURL,
						Valid:  track.ThumbnailURL != "",
					},
					BeatportID: pgtype.Int4{
						Int32: int32(track.ID),
						Valid: true,
					},
					BeatportSlug: pgtype.Text{
						String: track.Slug,
						Valid:  track.Slug != "",
					},
					MixName: pgtype.Text{
						String: track.MixName,
						Valid:  track.MixName != "",
					},
					ReleaseDate: utils.Text(track.ReleaseDate),
					ReleaseName: pgtype.Text{
						String: track.Release.Name,
						Valid:  track.Release.Name != "",
					},
					Genre: pgtype.Text{
						String: track.Genre.Name,
						Valid:  track.Genre.Name != "",
					},
					SubGenre: pgtype.Text{
						String: track.SubGenre.Name,
						Valid:  track.SubGenre.Name != "",
					},
					Bpm: pgtype.Int4{
						Int32: int32(track.BPM),
						Valid: track.BPM > 0,
					},
					MusicalKey: pgtype.Text{
						String: track.Key.Name,
						Valid:  track.Key.Name != "",
					},
					LengthMs: pgtype.Int4{
						Int32: int32(track.LengthMs),
						Valid: track.LengthMs > 0,
					},
				})

				switch {
				case err != nil:
					slog.Error("Failed to update song with beatport data",
						slog.String("name", track.Name), slog.Any("err", err))
				case rows > 0:
					index.ClaimBeatport(matchedSong, pgtype.Int4{Int32: int32(track.ID), Valid: true})
					updatedCount++
				default:
					// The row already held exactly this data. Counting it as an
					// update is what made every cycle report updated=~73 forever.
					skippedCount++
				}
				continue
			}

			// No similar song exists — insert new
			song, err := b.Queries.InsertBeatportSong(context.Background(), db.InsertBeatportSongParams{
				Name:        track.Name,
				Artists:     artistsStr,
				ReleaseDate: utils.Text(track.ReleaseDate),
				ThumbnailUrl: pgtype.Text{
					String: track.ThumbnailURL,
					Valid:  track.ThumbnailURL != "",
				},
				BeatportID: pgtype.Int4{
					Int32: int32(track.ID),
					Valid: true,
				},
				BeatportSlug: pgtype.Text{
					String: track.Slug,
					Valid:  track.Slug != "",
				},
				MixName: pgtype.Text{
					String: track.MixName,
					Valid:  track.MixName != "",
				},
				ReleaseName: pgtype.Text{
					String: track.Release.Name,
					Valid:  track.Release.Name != "",
				},
				Genre: pgtype.Text{
					String: track.Genre.Name,
					Valid:  track.Genre.Name != "",
				},
				SubGenre: pgtype.Text{
					String: track.SubGenre.Name,
					Valid:  track.SubGenre.Name != "",
				},
				Bpm: pgtype.Int4{
					Int32: int32(track.BPM),
					Valid: track.BPM > 0,
				},
				MusicalKey: pgtype.Text{
					String: track.Key.Name,
					Valid:  track.Key.Name != "",
				},
				LengthMs: pgtype.Int4{
					Int32: int32(track.LengthMs),
					Valid: track.LengthMs > 0,
				},
			})

			if err != nil {
				// A row already holds this (name, artists, release_date). Beatport
				// lists the same recording under several ids, so this is the normal
				// steady state, not a fault.
				if db.ErrorCode(err) == db.UniqueViolation {
					slog.Debug("Beatport track already stored",
						slog.String("name", track.Name), slog.String("artists", artistsStr))
					skippedCount++
					continue
				}
				slog.Error("Failed to insert beatport song",
					slog.String("name", track.Name), slog.Any("err", err))
				continue
			}

			newCount++
			finaliseNewSong(context.Background(), b, song, index)

			// Register it so later tracks in this same batch -- the six remixes of
			// one release arrive together -- match against it instead of inserting
			// another copy.
			index.Append(db.GetAllSongsForMatchingRow{
				ID:      song.ID,
				Name:    song.Name,
				Artists: song.Artists,
				MixName: song.MixName,
				BeatportID: pgtype.Int4{
					Int32: int32(track.ID),
					Valid: true,
				},
				Source:      song.Source,
				ReleaseDate: song.ReleaseDate,
			})

			// Announce only rows that have never been announced and are actually
			// recent. Beatport lists extended mixes and every individual remix as
			// separate tracks, so without the recency lock a catalogue re-read
			// floods the channel.
			// Stamp the watermark either way. A row we deliberately chose not to
			// announce is finished with, so leaving it NULL would erode the meaning
			// of the column: NULL should mean "still pending", not "old catalogue".
			if err := b.Queries.MarkSongAnnounced(context.Background(), song.ID); err != nil {
				slog.Error("Failed to mark song announced",
					slog.Int64("song_id", song.ID), slog.Any("err", err))
			}

			// Only recency decides here: the row was inserted a moment ago, so it
			// has never been announced by definition. The announced_at watermark is
			// what protects every other path from replaying the back catalogue.
			if isRecentRelease(utils.Text(track.ReleaseDate)) {
				// Build announcement embed
				embedBuilder := discord.NewEmbed().
					WithTitle(utils.SongHeading(artistsStr, track.Name, track.MixName)).
					WithColor(0x1DB954) // Green for beatport

				if track.ThumbnailURL != "" {
					embedBuilder = embedBuilder.WithImage(track.ThumbnailURL)
				}

				// Build footer with metadata
				var footerParts []string
				if track.ReleaseDate != "" {
					footerParts = append(footerParts, fmt.Sprintf("📅 %s", track.ReleaseDate))
				}
				if track.Genre.Name != "" {
					footerParts = append(footerParts, fmt.Sprintf("🎵 %s", track.Genre.Name))
				}
				if track.BPM > 0 {
					footerParts = append(footerParts, fmt.Sprintf("💓 %d BPM", track.BPM))
				}
				if track.Key.Name != "" {
					footerParts = append(footerParts, fmt.Sprintf("🔑 %s", track.Key.Name))
				}
				if track.LengthMs > 0 {
					footerParts = append(footerParts, fmt.Sprintf("⏱ %s", utils.FormatBeatportDuration(track.LengthMs)))
				}

				if len(footerParts) > 0 {
					embedBuilder = embedBuilder.WithFooter(strings.Join(footerParts, " | "), "")
				}

				announcementEmbed := embedBuilder

				// Lead with the beatport track link. This is built from the track id
				// rather than read from songs.beatport_url, which holds a RELEASE
				// URL from the STMPD dataset -- the track link is the more precise
				// destination for a beatport announcement.
				// Without a slug there is no track page to link to: /track/<id> is
				// not a route beatport serves. Lead with the song's other links
				// rather than with a button that 404s.
				buttons := utils.GetSongButtons(song)
				if track.Slug != "" {
					beatportURL := utils.BeatportTrackURL(track.Slug, int32(track.ID))
					buttons = utils.LeadWith(buttons, "Beatport", utils.BeatportButton(beatportURL))
				}
				components := utils.ChunkButtonRows(buttons)

				notifier.AddItem(utils.NotificationItem{
					Embed:      &announcementEmbed,
					Components: components,
					// A beatport-sourced row has metadata and almost never a
					// streaming link -- beatport supplies neither -- so this is the
					// announcement that gains the most from being corrected later.
					SongID:        song.ID,
					LinkSignature: utils.SongLinkSignature(song),
				})
			}
		}

		slog.Info("Beatport sync complete",
			slog.Int("new", newCount),
			slog.Int("updated", updatedCount),
			slog.Int("skipped", skippedCount))

		if err := notifier.Send(); err != nil {
			slog.Error("Failed to send batched beatport notifications", slog.Any("err", err))
		}
	}
}

// fillBeatportSlug stores the track slug on a row that has the beatport id but not the
// slug to build a URL from.
//
// Guarded on absence rather than written unconditionally: the slug is half of an
// editable link, and SetBeatportSlug's :execrows guard plus the locked_fields check make
// a no-op cost nothing, but there is no reason to offer a write at all when the row is
// already correct.
func fillBeatportSlug(b *stmpdbot.STMPDBot, song db.Song, slug string) {
	if slug == "" || song.BeatportSlug.Valid {
		return
	}
	n, err := b.Queries.SetBeatportSlug(context.Background(), db.SetBeatportSlugParams{
		ID: song.ID, BeatportSlug: utils.Text(slug),
	})
	if err != nil {
		slog.Error("Failed to store a beatport slug",
			slog.Int64("song_id", song.ID), slog.Any("err", err))
		return
	}
	if n > 0 {
		slog.Info("Filled in a missing beatport slug",
			slog.Int64("song_id", song.ID), slog.String("name", song.Name),
			slog.String("slug", slug))
	}
}
