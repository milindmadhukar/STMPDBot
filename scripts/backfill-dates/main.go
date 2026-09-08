// Command backfill-dates replaces the 1970-01-01 placeholder release dates with real
// ones, resolved from Apple's public lookup API.
//
// The placeholder is what the original importer wrote whenever it had no date. It is
// not harmless: release_date drives the announcement recency window, the "released"
// footer on a track card, and any date ordering, so a row stuck at 1970 sorts to the
// bottom of the catalogue forever.
//
// Resolution uses the numeric id already embedded in each row's own apple_music_url,
// not a search -- so the date returned belongs to the exact recording the row already
// links to, and no fuzzy matching is involved. Rows with no Apple link cannot be
// resolved this way and are reported, not guessed at.
//
// Under -recheck it does the opposite job and writes nothing: instead of the rows with
// no date it walks the rows that have one, and reports where the stored date disagrees
// with the recording the row links to. A missing date is obvious; a wrong one is not,
// and the wrong ones are the ones members notice.
//
// Idempotent, and never announces: this binary does not import the notifier.
package main

import (
	"context"
	"flag"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/scripts/internal/script"
	"github.com/milindmadhukar/STMPDBot/utils"
)

func main() {
	// Declared before Setup, which is what calls flag.Parse.
	recheck := flag.Bool("recheck", false,
		"audit the dates that already exist against Apple and report disagreements; writes nothing")

	env, ctx, cleanup := script.Setup("backfill-dates")
	defer cleanup()

	if *recheck {
		recheckDates(ctx, env)
		return
	}

	rows, err := env.Queries.GetSongsWithPlaceholderDate(ctx)
	if err != nil {
		script.Fatal("failed to load songs with placeholder dates", err)
	}
	slog.Info("Songs carrying the 1970-01-01 placeholder", slog.Int("count", len(rows)))

	client := utils.NewItunesClient()
	var resolved, viaSearch, unresolvable, notFound, merged, failed, suspicious, playlist int

	prog := script.NewProgress("resolve dates", len(rows))
	for _, row := range rows {
		prog.Step()
		if utils.IsApplePlaylistURL(row.AppleMusicUrl.String) {
			playlist++
			slog.Warn("apple link points at a playlist, not a release - the button sends users to the wrong place",
				slog.Int64("song_id", row.ID), slog.String("name", row.Name))
		}

		var result *utils.ItunesResult
		bySearch := false

		// Preferred path: the id already stored on the row. It names one exact
		// recording, so there is nothing to verify.
		if id := utils.AppleIDFromURL(row.AppleMusicUrl.String); id != "" && !utils.IsApplePlaylistURL(row.AppleMusicUrl.String) {
			r, err := client.Lookup(ctx, id)
			if err != nil {
				slog.Error("lookup failed",
					slog.Int64("song_id", row.ID), slog.String("apple_id", id), slog.Any("err", err))
				failed++
				continue
			}
			result = r
		}

		// Fallback: search by name and artists. This is a guess and is checked.
		if result == nil || result.Date() == "" {
			r, err := searchForRow(ctx, client, row)
			if err != nil {
				slog.Error("search failed",
					slog.Int64("song_id", row.ID), slog.String("name", row.Name), slog.Any("err", err))
				failed++
				continue
			}
			if r != nil {
				result, bySearch = r, true
			}
		}

		if result == nil {
			notFound++
			slog.Warn("no release found for this song",
				slog.Int64("song_id", row.ID), slog.String("name", row.Name), slog.String("artists", row.Artists))
			continue
		}

		date := result.Date()
		if date == "" {
			notFound++
			continue
		}
		if bySearch {
			viaSearch++
		}

		// The id came from this row, so a mismatch means the stored link is wrong,
		// not that the lookup is. Worth surfacing, not worth refusing: the date
		// still belongs to whatever the row actually links to.
		if !titlesAgree(row.Name, result.Title()) {
			suspicious++
			slog.Warn("stored link points at a differently-titled recording",
				slog.Int64("song_id", row.ID),
				slog.String("stored", row.Name),
				slog.String("apple", result.Title()),
				slog.String("apple_artist", result.ArtistName),
				slog.String("date", date))
		}

		if env.DryRun {
			slog.Info("would set release date",
				slog.Int64("song_id", row.ID), slog.String("name", row.Name),
				slog.String("artists", row.Artists), slog.String("date", date),
				slog.String("via", source(bySearch)),
				slog.String("apple_says", result.ArtistName+" - "+result.Title()))
			resolved++
			continue
		}

		n, err := env.Queries.SetSongReleaseDate(ctx, db.SetSongReleaseDateParams{
			ID: row.ID, ReleaseDate: utils.Text(date),
		})
		if err != nil {
			// The corrected date collided with unique_release, so a twin row already
			// holds this song at its real date. Fold this row into that one.
			if db.ErrorCode(err) == db.UniqueViolation {
				if mergeInto(ctx, env, row, date) {
					merged++
				} else {
					failed++
				}
				continue
			}
			slog.Error("failed to set release date",
				slog.Int64("song_id", row.ID), slog.Any("err", err))
			failed++
			continue
		}
		if n > 0 {
			resolved++
			slog.Info("release date resolved",
				slog.Int64("song_id", row.ID), slog.String("name", row.Name),
				slog.String("date", date))
		}
	}

	prog.Done()

	slog.Info("Date backfill complete",
		slog.Int("placeholder_rows", len(rows)),
		slog.Int("resolved", resolved),
		slog.Int("of_those_by_search", viaSearch),
		slog.Int("merged_into_twin", merged),
		slog.Int("no_apple_link", unresolvable),
		slog.Int("apple_link_is_a_playlist", playlist),
		slog.Int("apple_had_no_record", notFound),
		slog.Int("title_mismatch_warnings", suspicious),
		slog.Int("failed", failed))
}

// mergeInto folds row into the row that already holds this song at its real date.
func mergeInto(ctx context.Context, env *script.Env, row db.GetSongsWithPlaceholderDateRow, date string) bool {
	twin, err := env.Queries.GetSong(ctx, db.GetSongParams{
		Name: row.Name, Artists: row.Artists, ReleaseDate: utils.Text(date),
	})
	if err != nil {
		slog.Error("date collided but no twin row was found",
			slog.Int64("song_id", row.ID), slog.String("name", row.Name),
			slog.String("date", date), slog.Any("err", err))
		return false
	}

	slog.Info("merging placeholder row into its dated twin",
		slog.String("name", row.Name), slog.Int64("keep", twin.ID), slog.Int64("drop", row.ID))

	if err := env.Queries.MergeSongRows(ctx, db.MergeSongRowsParams{
		WinnerID: twin.ID, LoserID: row.ID,
	}); err != nil {
		slog.Error("failed to merge", slog.Int64("song_id", row.ID), slog.Any("err", err))
		return false
	}
	return true
}

// titlesAgree is a loose sanity check on the stored link, not a match gate.
func titlesAgree(stored, apple string) bool {
	a, _ := utils.SplitVariant(stored, "", "")
	b, _ := utils.SplitVariant(apple, "", "")
	if a == "" || b == "" {
		return true
	}
	return strings.Contains(a, b) || strings.Contains(b, a) || utils.IsCloseMatch(a, b, 0.80)
}

// searchForRow asks Apple for a song by name and artists, and returns a result only
// if it actually describes this row.
//
// Search always answers with its nearest match, even when Apple holds nothing for the
// query, so an unverified result is worse than none: it writes a confident, wrong
// date. The earliest verified match wins, since a song's own release predates the
// compilations and re-issues it later appears on.
func searchForRow(ctx context.Context, client *utils.ItunesClient, row db.GetSongsWithPlaceholderDateRow) (*utils.ItunesResult, error) {
	results, err := client.Search(ctx, row.Artists+" "+row.Name, 12)
	if err != nil {
		return nil, err
	}

	var best *utils.ItunesResult
	for i := range results {
		r := results[i]
		if r.Date() == "" || !utils.SameRecording(row.Name, row.Artists, r.Title(), r.ArtistName) {
			continue
		}
		if best == nil || r.Date() < best.Date() {
			best = &results[i]
		}
	}
	return best, nil
}

func source(bySearch bool) string {
	if bySearch {
		return "search (verified)"
	}
	return "stored apple id"
}

// recheckGapDays is how far a stored date may sit from Apple's before it is worth a
// person's attention.
//
// Not zero, and not one: beatport publishes a track the day a label delivers it and
// Apple lists the day it goes on sale, so a day or two of disagreement is the normal
// state of an honest row and reporting it would bury the real defects. A month apart
// is not a rounding difference -- it is two different events.
const recheckGapDays = 30

// dateDisagreement is one row whose stored date does not match the recording it links to.
type dateDisagreement struct {
	row      db.GetSongsWithACheckableDateRow
	appleDay string
	gapDays  int
	// linkedTitle is what Apple calls the recording this row points at. When it is
	// not this song, the defect is the stored link, and the date is only its symptom.
	linkedTitle string
	release     string
	titleAgrees bool
}

// recheckDates walks every row that has both a date and an Apple link and reports the
// ones where the two disagree. It writes nothing, deliberately.
//
// Correcting these is a person's job and not a pass's. Apple's date belongs to
// whichever release the row happens to link to, so a remix row pointed at the
// original single, or a track pointed at the compilation it was later collected on,
// disagrees for a reason that is not "the stored date is wrong" -- and rewriting
// those automatically would replace a handful of wrong dates with a hundred. What the
// pass can do honestly is find the disagreements and put the evidence next to each
// one.
func recheckDates(ctx context.Context, env *script.Env) {
	rows, err := env.Queries.GetSongsWithACheckableDate(ctx)
	if err != nil {
		script.Fatal("failed to load songs with a checkable date", err)
	}
	slog.Info("Rows with a date to check", slog.Int("count", len(rows)))

	client := utils.NewItunesClient()
	var agreed, playlist, notFound, failed int
	var found []dateDisagreement

	prog := script.NewProgress("recheck dates", len(rows))
	for _, row := range rows {
		prog.Step()

		if utils.IsApplePlaylistURL(row.AppleMusicUrl.String) {
			playlist++
			continue
		}
		id := utils.AppleIDFromURL(row.AppleMusicUrl.String)
		if id == "" {
			notFound++
			continue
		}

		result, err := client.Lookup(ctx, id)
		if err != nil {
			slog.Error("lookup failed",
				slog.Int64("song_id", row.ID), slog.String("apple_id", id), slog.Any("err", err))
			failed++
			continue
		}
		if result == nil || result.Date() == "" {
			notFound++
			continue
		}

		gap, ok := daysApart(row.ReleaseDate.String, result.Date())
		if !ok {
			notFound++
			continue
		}
		if gap <= recheckGapDays {
			agreed++
			continue
		}

		found = append(found, dateDisagreement{
			row:         row,
			appleDay:    result.Date(),
			gapDays:     gap,
			linkedTitle: result.ArtistName + " - " + result.Title(),
			release:     result.CollectionTitle(),
			titleAgrees: titlesAgree(row.Name, result.Title()),
		})
	}
	prog.Done()

	// Widest disagreement first: a row that is years out is a different kind of
	// problem from one that is a season out, and it is the one to read first.
	sort.Slice(found, func(i, j int) bool { return found[i].gapDays > found[j].gapDays })

	badLinks := 0
	for _, d := range found {
		mix := ""
		if d.row.MixName.String != "" {
			mix = " (" + d.row.MixName.String + ")"
		}
		if !d.titleAgrees {
			badLinks++
		}
		slog.Warn("stored date disagrees with the linked recording",
			slog.Int64("song_id", d.row.ID),
			slog.String("song", d.row.Artists+" - "+d.row.Name+mix),
			slog.String("stored", d.row.ReleaseDate.String),
			slog.String("apple", d.appleDay),
			slog.Int("gap_days", d.gapDays),
			slog.String("apple_calls_it", d.linkedTitle),
			slog.String("on_release", d.release),
			slog.Bool("link_is_this_song", d.titleAgrees))
	}

	slog.Info("Date recheck complete",
		slog.Int("checked", len(rows)),
		slog.Int("agreed", agreed),
		slog.Int("disagreed", len(found)),
		slog.Int("of_those_linked_to_another_recording", badLinks),
		slog.Int("apple_link_is_a_playlist", playlist),
		slog.Int("apple_had_no_date", notFound),
		slog.Int("failed", failed),
		slog.String("threshold", "more than "+strconv.Itoa(recheckGapDays)+" days apart"))

	if len(found) == 0 {
		slog.Info("Every checkable date agrees with the recording it links to")
		return
	}
	slog.Info("Nothing was written. Correct a date on the song's dashboard page, " +
		"which locks the column so no pass overwrites it, and re-run to confirm.")
}

// daysApart is the absolute distance in days between two ISO days, or false when
// either is not one.
func daysApart(a, b string) (int, bool) {
	first, err := time.Parse(time.DateOnly, a)
	if err != nil {
		return 0, false
	}
	second, err := time.Parse(time.DateOnly, b)
	if err != nil {
		return 0, false
	}
	days := int(first.Sub(second).Hours() / 24)
	if days < 0 {
		days = -days
	}
	return days, true
}
