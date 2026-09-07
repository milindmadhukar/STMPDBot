//go:build integration

package db_test

// The song catalogue against a real Postgres. Two ingestion paths (Beatport and
// STMPD) write into one songs table and dedupe against each other, and the
// uniqueness rules that keep that honest live in the schema, not in Go, so they
// can only be verified here.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/utils"
)

// uniqueSuffix keeps rows from different tests apart, so they can run in
// parallel against one database without colliding on unique_release.
var uniqueSuffix atomic.Int64

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
func int4(i int32) pgtype.Int4  { return pgtype.Int4{Int32: i, Valid: true} }

// testSongName returns a name no other test will use.
func testSongName(t *testing.T, base string) string {
	t.Helper()
	return fmt.Sprintf("%s #%d", base, uniqueSuffix.Add(1))
}

// insertBeatportSong inserts a song and removes it when the test ends.
func insertBeatportSong(t *testing.T, q *db.Queries, arg db.InsertBeatportSongParams) db.Song {
	t.Helper()

	song, err := q.InsertBeatportSong(context.Background(), arg)
	if err != nil {
		t.Fatalf("InsertBeatportSong failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, song.ID) })
	return song
}

func deleteSong(t *testing.T, id int64) {
	t.Helper()

	if _, err := testPool.Exec(context.Background(), "DELETE FROM songs WHERE id = $1", id); err != nil {
		t.Errorf("failed to clean up song %d: %v", id, err)
	}
}

// beatportID allocates an id no other test will use.
func beatportID() int32 { return int32(900_000_000 + uniqueSuffix.Add(1)) }

func TestInsertBeatportSong_RoundTrip(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	name := testSongName(t, "Animals")
	bpID := beatportID()

	inserted := insertBeatportSong(t, q, db.InsertBeatportSongParams{
		Name:         name,
		Artists:      "Martin Garrix",
		ReleaseDate:  text("2013-06-17"),
		ThumbnailUrl: text("https://example.com/a.jpg"),
		BeatportID:   int4(bpID),
		MixName:      text("Original Mix"),
		ReleaseName:  text("Animals"),
		Genre:        text("Big Room"),
		SubGenre:     text("Mainstage"),
		Bpm:          int4(128),
		MusicalKey:   text("F Minor"),
		LengthMs:     int4(303000),
	})

	if inserted.Source != "beatport" {
		t.Errorf("source = %q, want %q", inserted.Source, "beatport")
	}
	if inserted.BeatportUpdated {
		t.Error("a freshly inserted song should not be marked beatport_updated")
	}

	got, err := q.GetSongByBeatportID(ctx, int4(bpID))
	if err != nil {
		t.Fatalf("GetSongByBeatportID failed: %v", err)
	}

	if got.ID != inserted.ID {
		t.Errorf("got song %d, want %d", got.ID, inserted.ID)
	}
	if got.Name != name {
		t.Errorf("name = %q, want %q", got.Name, name)
	}
	if got.Bpm.Int32 != 128 {
		t.Errorf("bpm = %d, want 128", got.Bpm.Int32)
	}
	if got.LengthMs.Int32 != 303000 {
		t.Errorf("length_ms = %d, want 303000", got.LengthMs.Int32)
	}
}

func TestInsertRelease_RoundTripOnTheNaturalKey(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	name := testSongName(t, "Kangaroo")
	const artists, releaseDate = "Julian Jordan", "2026-01-01"

	inserted, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name:        name,
		Artists:     artists,
		ReleaseDate: text(releaseDate),
		SpotifyUrl:  text("https://open.spotify.com/track/x"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, inserted.ID) })

	if inserted.Source != "stmpd" {
		t.Errorf("source = %q, want the stmpd default", inserted.Source)
	}

	// GetSong looks up on (name, artists, release_date), which is the key the
	// autocomplete round-trip and the ingestion conflict check both use.
	got, err := q.GetSong(ctx, db.GetSongParams{
		Name: name, Artists: artists, ReleaseDate: text(releaseDate),
	})
	if err != nil {
		t.Fatalf("GetSong failed: %v", err)
	}
	if got.ID != inserted.ID {
		t.Errorf("got song %d, want %d", got.ID, inserted.ID)
	}
}

func TestDoesSongExist(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	name := testSongName(t, "Poison")
	const artists, releaseDate = "Martin Garrix", "2015-01-01"

	exists, err := q.DoesSongExist(ctx, db.DoesSongExistParams{
		Name: name, Artists: artists, ReleaseDate: text(releaseDate),
	})
	if err != nil {
		t.Fatalf("DoesSongExist failed: %v", err)
	}
	if exists {
		t.Fatal("the song exists before it was inserted")
	}

	song, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name: name, Artists: artists, ReleaseDate: text(releaseDate),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, song.ID) })

	exists, err = q.DoesSongExist(ctx, db.DoesSongExistParams{
		Name: name, Artists: artists, ReleaseDate: text(releaseDate),
	})
	if err != nil {
		t.Fatalf("DoesSongExist failed: %v", err)
	}
	if !exists {
		t.Error("the song does not exist after being inserted")
	}

	// The date is part of the key, so the same track under a different date is
	// a different row. This is why the STMPD fetcher keeps writing <year>-01-01.
	exists, err = q.DoesSongExist(ctx, db.DoesSongExistParams{
		Name: name, Artists: artists, ReleaseDate: text("2020-01-01"),
	})
	if err != nil {
		t.Fatalf("DoesSongExist failed: %v", err)
	}
	if exists {
		t.Error("a different release date matched an existing song; the STMPD " +
			"fetcher relies on the date being part of the key")
	}
}

// unique_release is what stops the fetchers re-inserting a song they have
// already stored. It is a schema constraint, so only a real database proves it.
func TestUniqueRelease_RejectsADuplicate(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	name := testSongName(t, "Byte")
	params := db.InsertReleaseParams{
		Name: name, Artists: "Martin Garrix", ReleaseDate: text("2026-01-01"),
	}

	song, err := q.InsertRelease(ctx, params)
	if err != nil {
		t.Fatalf("the first insert failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, song.ID) })

	_, err = q.InsertRelease(ctx, params)
	if err == nil {
		t.Fatal("inserting the same release twice was allowed")
	}

	// ErrorCode is how the handlers tell a conflict from a real failure; this
	// exercises it against a driver error rather than a synthetic one.
	if got := db.ErrorCode(err); got != db.UniqueViolation {
		t.Errorf("ErrorCode() = %q, want the unique violation %q", got, db.UniqueViolation)
	}
}

// The partial unique index means one Beatport track maps to at most one row,
// while the many songs with no Beatport id stay unconstrained.
func TestBeatportID_IsUniqueButNullable(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	bpID := beatportID()

	first := insertBeatportSong(t, q, db.InsertBeatportSongParams{
		Name: testSongName(t, "First"), Artists: "Martin Garrix",
		ReleaseDate: text("2026-01-01"), BeatportID: int4(bpID),
	})
	_ = first

	_, err := q.InsertBeatportSong(ctx, db.InsertBeatportSongParams{
		Name: testSongName(t, "Second"), Artists: "Martin Garrix",
		ReleaseDate: text("2026-01-02"), BeatportID: int4(bpID),
	})
	if err == nil {
		t.Fatal("two songs were allowed to share a beatport id")
	}
	if got := db.ErrorCode(err); got != db.UniqueViolation {
		t.Errorf("ErrorCode() = %q, want %q", got, db.UniqueViolation)
	}

	// Songs with no Beatport id are exempt from the index, so several can coexist.
	for i := range 2 {
		song, err := q.InsertRelease(ctx, db.InsertReleaseParams{
			Name:        testSongName(t, fmt.Sprintf("No Beatport ID %d", i)),
			Artists:     "STMPD",
			ReleaseDate: text("2026-01-01"),
		})
		if err != nil {
			t.Fatalf("a song with no beatport id was rejected: %v", err)
		}
		t.Cleanup(func() { deleteSong(t, song.ID) })
	}
}

// GetAllSongsForMatching feeds findSimilarExistingSong, so the columns it
// returns are the ones the dedupe key is built from.
func TestGetAllSongsForMatching_ReturnsTheDedupeColumns(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	name := testSongName(t, "Matching")
	bpID := beatportID()

	inserted := insertBeatportSong(t, q, db.InsertBeatportSongParams{
		Name: name, Artists: "Martin Garrix",
		ReleaseDate: text("2026-01-01"), BeatportID: int4(bpID),
	})

	rows, err := q.GetAllSongsForMatching(ctx)
	if err != nil {
		t.Fatalf("GetAllSongsForMatching failed: %v", err)
	}

	for _, row := range rows {
		if row.ID != inserted.ID {
			continue
		}

		if row.Name != name {
			t.Errorf("name = %q, want %q", row.Name, name)
		}
		if row.Artists != "Martin Garrix" {
			t.Errorf("artists = %q, want %q", row.Artists, "Martin Garrix")
		}
		if !row.BeatportID.Valid || row.BeatportID.Int32 != bpID {
			t.Errorf("beatport id = %+v, want %d", row.BeatportID, bpID)
		}
		if row.Source != "beatport" {
			t.Errorf("source = %q, want %q", row.Source, "beatport")
		}
		return
	}

	t.Fatalf("song %d is missing from the matching corpus", inserted.ID)
}

func TestUpdateSongWithBeatportData(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	name := testSongName(t, "To Enrich")
	song, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name: name, Artists: "Martin Garrix", ReleaseDate: text("2026-01-01"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, song.ID) })

	bpID := beatportID()
	// :execrows now, so a cycle that changes nothing reports zero rows instead of
	// rewriting the same rows forever. The count is part of the contract.
	rows, err := q.UpdateSongWithBeatportData(ctx, db.UpdateSongWithBeatportDataParams{
		ID:         song.ID,
		Name:       name,
		Artists:    "Martin Garrix",
		BeatportID: int4(bpID),
		MixName:    text("Extended Mix"),
		Genre:      text("Big Room"),
		Bpm:        int4(128),
		LengthMs:   int4(303000),
	})
	if err != nil {
		t.Fatalf("UpdateSongWithBeatportData failed: %v", err)
	}
	if rows != 1 {
		t.Fatalf("UpdateSongWithBeatportData wrote %d rows, want 1", rows)
	}

	got, err := q.GetSongByID(ctx, song.ID)
	if err != nil {
		t.Fatalf("GetSongByID failed: %v", err)
	}

	if !got.BeatportID.Valid || got.BeatportID.Int32 != bpID {
		t.Errorf("beatport id = %+v, want %d", got.BeatportID, bpID)
	}
	if got.Bpm.Int32 != 128 {
		t.Errorf("bpm = %d, want 128", got.Bpm.Int32)
	}
	// Enriching an existing row must mark it done, or every cycle would rewrite it.
	if !got.BeatportUpdated {
		t.Error("the song was not marked beatport_updated after enrichment")
	}
}

func TestMarkBeatportUpdated(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	song := insertBeatportSong(t, q, db.InsertBeatportSongParams{
		Name: testSongName(t, "To Mark"), Artists: "Martin Garrix",
		ReleaseDate: text("2026-01-01"), BeatportID: int4(beatportID()),
	})

	if err := q.MarkBeatportUpdated(ctx, song.ID); err != nil {
		t.Fatalf("MarkBeatportUpdated failed: %v", err)
	}

	got, err := q.GetSongByID(ctx, song.ID)
	if err != nil {
		t.Fatalf("GetSongByID failed: %v", err)
	}
	if !got.BeatportUpdated {
		t.Error("the song was not marked beatport_updated")
	}
}

func TestUpdateSongWithStmpdLinks(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	song := insertBeatportSong(t, q, db.InsertBeatportSongParams{
		Name: testSongName(t, "To Link"), Artists: "Martin Garrix",
		ReleaseDate: text("2026-01-01"), BeatportID: int4(beatportID()),
	})

	// :execrows, so a no-op cycle reports zero rows rather than claiming a write.
	linked, err := q.UpdateSongWithStmpdLinks(ctx, db.UpdateSongWithStmpdLinksParams{
		ID:            song.ID,
		SpotifyUrl:    text("https://open.spotify.com/track/x"),
		AppleMusicUrl: text("https://music.apple.com/x"),
		YoutubeUrl:    text("https://youtu.be/x"),
	})
	if err != nil {
		t.Fatalf("UpdateSongWithStmpdLinks failed: %v", err)
	}
	if linked != 1 {
		t.Fatalf("UpdateSongWithStmpdLinks wrote %d rows, want 1", linked)
	}

	got, err := q.GetSongByID(ctx, song.ID)
	if err != nil {
		t.Fatalf("GetSongByID failed: %v", err)
	}

	if got.SpotifyUrl.String != "https://open.spotify.com/track/x" {
		t.Errorf("spotify = %q, want the value that was set", got.SpotifyUrl.String)
	}
	if got.YoutubeUrl.String != "https://youtu.be/x" {
		t.Errorf("youtube = %q, want the value that was set", got.YoutubeUrl.String)
	}
}

func TestGetSongsLike(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	name := testSongName(t, "Searchable Anthem")
	song, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name: name, Artists: "Findable Artist", ReleaseDate: text("2026-01-01"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, song.ID) })

	// The handlers fold the member's input exactly like this.
	rows, err := q.GetSongsLike(ctx, utils.SearchTerms(name))
	if err != nil {
		t.Fatalf("GetSongsLike failed: %v", err)
	}

	// GetSongsLike returns exactly the (name, artists, release_date) triple the
	// autocomplete handlers serialise back into the choice value.
	found := false
	for _, row := range rows {
		if row.Name == song.Name && row.Artists == song.Artists {
			found = true
		}
	}
	if !found {
		t.Errorf("%q did not match itself", name)
	}

	// Terms are matched one at a time against a folded haystack, so a searcher can
	// name the artist and part of the title in whichever order they remember them.
	// The old form was one contiguous LIKE and only the exact "artists - name" phrase
	// matched, which is why a song credited to three acts read as missing.
	for _, query := range []string{
		"findable artist searchable",
		"searchable findable",
		"FINDABLE   anthem",
	} {
		rows, err = q.GetSongsLike(ctx, utils.SearchTerms(query))
		if err != nil {
			t.Fatalf("GetSongsLike(%q) failed: %v", query, err)
		}
		found := false
		for _, row := range rows {
			if row.Name == song.Name {
				found = true
			}
		}
		if !found {
			t.Errorf("%q did not find the song", query)
		}
	}
}

// Member input used to reach LIKE unescaped, so typing "_" got the
// single-character wildcard and "%" alone matched the whole catalogue. Folding the
// query through utils.SearchTerms fixes that as a side effect rather than by escaping:
// NormalizeToken keeps only letters and digits, so a wildcard cannot survive into the
// pattern at all.
func TestGetSongsLike_MemberInputCannotSmuggleWildcards(t *testing.T) {
	t.Parallel()

	q := queries(t)

	ctx := context.Background()

	name := testSongName(t, "Escaping Probe")
	song, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name: name, Artists: "Escaping Artist", ReleaseDate: text("2026-01-01"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, song.ID) })

	// "_" is dropped by the folding rather than passed through as a wildcard, so this
	// is now a search for the two words either side of it -- which does still find the
	// song, correctly and for the right reason.
	rows, err := q.GetSongsLike(ctx, utils.SearchTerms("escaping_artist"))
	if err != nil {
		t.Fatalf("GetSongsLike failed: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.Name == name {
			found = true
		}
	}
	if !found {
		t.Error("folding the query lost the song entirely")
	}

	// A bare "%" used to match the whole catalogue. It now folds to no terms at all,
	// which the query treats as "no restriction" -- so it still returns rows, but as
	// an unfiltered listing rather than as an injected wildcard.
	for _, hostile := range []string{"%", "_", "%%%", "\\"} {
		if terms := utils.SearchTerms(hostile); len(terms) != 0 {
			t.Errorf("SearchTerms(%q) = %v; a wildcard reached the LIKE pattern", hostile, terms)
		}
	}
}

func TestGetSongByBeatportID_NotFound(t *testing.T) {
	t.Parallel()

	q := queries(t)

	_, err := q.GetSongByBeatportID(context.Background(), int4(-1))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("error = %v, want pgx.ErrNoRows", err)
	}
	// The handlers compare against this alias rather than pgx directly.
	if !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("error = %v, want it to match db.ErrRecordNotFound", err)
	}
}

// The radio only plays songs with a YouTube link, and skips anything over ten
// minutes. That length filter existed in songs.sql but had never been
// regenerated into the Go code, so it was not actually applied.
func TestGetRandomSongForRadio_ExcludesLongTracks(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	long := insertBeatportSong(t, q, db.InsertBeatportSongParams{
		Name: testSongName(t, "Very Long Mix"), Artists: "Martin Garrix",
		ReleaseDate: text("2026-01-01"), BeatportID: int4(beatportID()),
		LengthMs: int4(3_600_000), // an hour
	})

	// A short, eligible song so the query has something to return; without one
	// this would pass vacuously on an empty catalogue.
	short := insertBeatportSong(t, q, db.InsertBeatportSongParams{
		Name: testSongName(t, "Radio Edit"), Artists: "Martin Garrix",
		ReleaseDate: text("2026-01-01"), BeatportID: int4(beatportID()),
		LengthMs: int4(180_000), // three minutes
	})

	for _, id := range []int64{long.ID, short.ID} {
		if _, err := testPool.Exec(ctx,
			"UPDATE songs SET youtube_url = $2 WHERE id = $1",
			id, "https://youtu.be/x"); err != nil {
			t.Fatalf("failed to set a youtube url: %v", err)
		}
	}

	// Sample repeatedly; the query orders randomly, so one draw proves little.
	for range 50 {
		got, err := q.GetRandomSongForRadio(ctx)
		if err != nil {
			t.Fatalf("GetRandomSongForRadio failed: %v", err)
		}
		if got.ID == long.ID {
			t.Fatalf("the radio picked %q, which is an hour long; the "+
				"length_ms <= 600000 filter is not being applied", got.Name)
		}
	}
}

// Discord caps an option value at 100 characters, and the autocomplete handlers
// put the song id there.
//
// They used to marshal {name, artists, release_date} instead, which a long title
// pushed past the cap: selecting such a song failed the whole interaction with
// 50035 Invalid Form Body. The id is bounded by the column width, so the longest
// title in the catalogue cannot reproduce that.
func TestSongChoiceValue_FitsDiscordsOptionValueLimit(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	name := testSongName(t, strings.Repeat("A Very Long Song Title ", 3))
	song, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name:        name,
		Artists:     "An Artist With A Rather Long Name, And A Collaborator",
		ReleaseDate: text("2026-01-01"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, song.ID) })

	value := strconv.FormatInt(song.ID, 10)
	if len(value) > 100 {
		t.Errorf("choice value for song %d is %d characters, over Discord's limit of 100",
			song.ID, len(value))
	}

	// The old shape, kept here to show what the id replaced: on this row it is
	// already over the cap.
	legacy := fmt.Sprintf(`{"name":%q,"artists":%q,"release_date":%q}`,
		song.Name, song.Artists, song.ReleaseDate.String)
	if len(legacy) <= 100 {
		t.Skip("this title is no longer long enough to have tripped the old limit")
	}
}

// The quiz shows lyrics and asks for the song's title, so it may only serve records
// that are actually the monitored acts'. Crediting one of them is not enough: a remix
// credit names both acts, so "Can't Feel My Face" came back as "The Weeknd, Martin
// Garrix" and the quiz spent The Weeknd's words asking people to name a Martin Garrix
// song. "Cocoon" is 070 Shake's and did the same.
//
// Which side of the remix our act is on is the whole distinction, so both directions
// are pinned here. Our act in the remixer slot means the record belongs to whoever we
// remixed; anyone else there means one of our own songs in someone else's hands, whose
// lyrics are still ours to quiz on.
//
// Both draws are random, so the test samples rather than asking once. It assumes the
// dedicated test database of the package docs, where the pool it is drawing from is
// the handful of rows below.
func TestGetRandomSongWithLyrics_ExcludesOurRemixesOfOtherActsRecords(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	// Our act as the remixer: the record, and the words, are The Weeknd's.
	theirs := insertBeatportSong(t, q, db.InsertBeatportSongParams{
		Name: testSongName(t, "Can't Feel My Face"), Artists: "The Weeknd, Martin Garrix",
		ReleaseDate: text("2026-01-01"), BeatportID: int4(beatportID()),
		MixName: text("Martin Garrix Remix"),
	})

	// Our act as the artist, someone else remixing: still our song and our words.
	ours := insertBeatportSong(t, q, db.InsertBeatportSongParams{
		Name: testSongName(t, "La La La"), Artists: "AREA21, Drove",
		ReleaseDate: text("2026-01-01"), BeatportID: int4(beatportID()),
		MixName: text("Drove Remix"),
	})

	// An ordinary song, so neither loop below can pass on an empty pool.
	plain := insertBeatportSong(t, q, db.InsertBeatportSongParams{
		Name: testSongName(t, "Animals"), Artists: "Martin Garrix",
		ReleaseDate: text("2026-01-01"), BeatportID: int4(beatportID()),
	})

	for _, id := range []int64{theirs.ID, ours.ID, plain.ID} {
		if _, err := testPool.Exec(ctx,
			"UPDATE songs SET lyrics = $2 WHERE id = $1",
			id, "we go where nobody knows"); err != nil {
			t.Fatalf("failed to set lyrics: %v", err)
		}
	}

	var sawOurs bool
	for range 200 {
		got, err := q.GetRandomSongWithLyrics(ctx)
		if err != nil {
			t.Fatalf("GetRandomSongWithLyrics failed: %v", err)
		}
		if got.ID == theirs.ID {
			t.Fatalf("the quiz served %q (%s), a remix of another act's record; "+
				"its lyrics are not the monitored acts' to quiz on", got.Name, got.MixName.String)
		}
		if got.ID == ours.ID {
			sawOurs = true
		}
	}
	if !sawOurs {
		t.Errorf("%q (%s) never came up: a remix of one of our own songs is still "+
			"ours, and excluding it would cost the quiz real material",
			ours.Name, ours.MixName.String)
	}

	// The easy query narrows which acts qualify, not what counts as their song. The
	// AREA21 row is out of its scope, so only the exclusion is checked here.
	for range 200 {
		got, err := q.GetRandomSongWithLyricsEasy(ctx)
		if err != nil {
			t.Fatalf("GetRandomSongWithLyricsEasy failed: %v", err)
		}
		if got.ID == theirs.ID {
			t.Fatalf("the easy quiz served %q (%s), a remix of another act's record",
				got.Name, got.MixName.String)
		}
	}
}

// locked_fields is what makes hand-correcting the catalogue worth doing.
//
// Four automated writers rewrite these same columns forever -- the STMPD and Beatport
// syncs every fifteen minutes, the Apple enrichment hourly, the LRCLIB sweep daily --
// so before this column existed a correction typed into the dashboard survived until
// whichever of them ran next. These tests pin that a locked column is skipped, that the
// row still reports zero writes (a guard on the SET list alone would leave the WHERE
// matching and reintroduce the write churn the IS DISTINCT FROM guards exist to kill),
// and that unlocking hands the column back.
func TestLockedFieldsBlockAutomatedWrites(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	name := testSongName(t, "Locked Row")
	song, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name: name, Artists: "Locking Artist", ReleaseDate: text("2026-01-01"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, song.ID) })

	const handSet = "https://cdn.sanity.io/hand-corrected.jpg"
	locked, err := q.DashUpdateSong(ctx, db.DashUpdateSongParams{
		ID: song.ID, Name: name, Artists: "Locking Artist",
		ReleaseDate:  text("2026-01-01"),
		ThumbnailUrl: text(handSet),
		LockedFields: []string{"thumbnail_url", "release_date"},
	})
	if err != nil {
		t.Fatalf("DashUpdateSong failed: %v", err)
	}
	if locked.ThumbnailUrl.String != handSet {
		t.Fatalf("artwork = %q, want the hand-set value", locked.ThumbnailUrl.String)
	}

	// The hourly Apple enrichment. Its own guard is "only fill an empty cover", so
	// give it an empty one to prove the lock is what stops it rather than that guard.
	if _, err = q.ClearSongArtwork(ctx, song.ID); err != nil {
		t.Fatalf("ClearSongArtwork failed: %v", err)
	}
	if got, gErr := q.GetSongByID(ctx, song.ID); gErr != nil {
		t.Fatal(gErr)
	} else if got.ThumbnailUrl.String != handSet {
		t.Error("fix-shared-artwork cleared a locked cover")
	}

	rows, err := q.SetSongArtwork(ctx, db.SetSongArtworkParams{
		ID: song.ID, ThumbnailUrl: text("https://example.invalid/robot.jpg"),
	})
	if err != nil {
		t.Fatalf("SetSongArtwork failed: %v", err)
	}
	if rows != 0 {
		t.Errorf("SetSongArtwork wrote %d rows over a locked cover, want 0", rows)
	}

	// The date backfill.
	if rows, err = q.SetSongReleaseDate(ctx, db.SetSongReleaseDateParams{
		ID: song.ID, ReleaseDate: text("1999-12-31"),
	}); err != nil {
		t.Fatalf("SetSongReleaseDate failed: %v", err)
	} else if rows != 0 {
		t.Errorf("SetSongReleaseDate wrote %d rows over a locked date, want 0", rows)
	}

	// The fifteen-minute Beatport sync, which rewrites a dozen columns at once. It must
	// skip the two locked ones and still apply the rest.
	if _, err = q.UpdateSongWithBeatportData(ctx, db.UpdateSongWithBeatportDataParams{
		ID: song.ID, Name: name, Artists: "Locking Artist",
		ReleaseDate:  text("1999-12-31"),
		ThumbnailUrl: text("https://example.invalid/robot.jpg"),
		BeatportID:   int4(beatportID()),
		Bpm:          int4(128),
	}); err != nil {
		t.Fatalf("UpdateSongWithBeatportData failed: %v", err)
	}

	got, err := q.GetSongByID(ctx, song.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ThumbnailUrl.String != handSet {
		t.Errorf("the Beatport sync overwrote a locked cover: %q", got.ThumbnailUrl.String)
	}
	if got.ReleaseDate.String != "2026-01-01" {
		t.Errorf("the Beatport sync overwrote a locked date: %q", got.ReleaseDate.String)
	}
	if got.Bpm.Int32 != 128 {
		t.Errorf("the Beatport sync skipped an UNLOCKED column: bpm = %d, want 128", got.Bpm.Int32)
	}

	// A second identical cycle must report no writes at all. Guarding only the SET
	// list would leave the WHERE matching every time, and the churn the :execrows
	// guards exist to prevent would be back.
	rows, err = q.UpdateSongWithBeatportData(ctx, db.UpdateSongWithBeatportDataParams{
		ID: song.ID, Name: name, Artists: "Locking Artist",
		ReleaseDate:  text("1999-12-31"),
		ThumbnailUrl: text("https://example.invalid/robot.jpg"),
		BeatportID:   got.BeatportID,
		Bpm:          int4(128),
	})
	if err != nil {
		t.Fatalf("UpdateSongWithBeatportData failed: %v", err)
	}
	if rows != 0 {
		t.Errorf("a repeat cycle over a locked row wrote %d rows, want 0 -- the lock "+
			"guard is missing from the WHERE clause", rows)
	}

	// The fifteen-minute STMPD sync, which is the writer that would otherwise re-arm
	// an announcement: correcting a locked release_date must not make an old row look
	// like news and post it to every server again.
	if _, err = q.UpdateSongWithStmpdRelease(ctx, db.UpdateSongWithStmpdReleaseParams{
		ID:           song.ID,
		ReleaseDate:  text("1999-12-31"),
		Title:        text("Renamed By The Sync"),
		ThumbnailUrl: text("https://example.invalid/robot.jpg"),
		SpotifyUrl:   text("https://open.spotify.com/track/synced"),
	}); err != nil {
		t.Fatalf("UpdateSongWithStmpdRelease failed: %v", err)
	}
	if got, gErr := q.GetSongByID(ctx, song.ID); gErr != nil {
		t.Fatal(gErr)
	} else {
		if got.ReleaseDate.String != "2026-01-01" {
			t.Errorf("the STMPD sync overwrote a locked date: %q", got.ReleaseDate.String)
		}
		if got.ThumbnailUrl.String != handSet {
			t.Errorf("the STMPD sync overwrote a locked cover: %q", got.ThumbnailUrl.String)
		}
		if got.SpotifyUrl.String != "https://open.spotify.com/track/synced" {
			t.Error("the STMPD sync skipped an UNLOCKED link")
		}
		if got.Name != "Renamed By The Sync" {
			t.Errorf("the STMPD sync skipped the UNLOCKED name: %q", got.Name)
		}
	}

	// Unlocking hands the column straight back to automation.
	if _, err = q.DashUnlockSongField(ctx, db.DashUnlockSongFieldParams{
		ID: song.ID, Field: "thumbnail_url",
	}); err != nil {
		t.Fatalf("DashUnlockSongField failed: %v", err)
	}
	if rows, err = q.ClearSongArtwork(ctx, song.ID); err != nil {
		t.Fatalf("ClearSongArtwork failed: %v", err)
	} else if rows != 1 {
		t.Errorf("ClearSongArtwork wrote %d rows after unlocking, want 1", rows)
	}
}

// Lyrics are the one column that exists nowhere else -- they are hand-entered or
// verified against a duration, and nothing can re-derive them. A lock has to survive a
// merge, or folding a duplicate away silently hands its hand-corrected columns back to
// A merge must survive the winner taking an identity the loser is still holding.
//
// unique_release is (name, artists, mix_name, release_date) NULLS NOT DISTINCT, and
// MergeSongRows COALESCEs the loser's mix_name onto a winner that has none. Those two
// facts together mean a pair differing only in that one names a rendition and the
// other does not -- beatport files nearly every plain release as an "Extended Mix",
// so this is the commonest duplicate in the catalogue -- produced a 23505 and left
// the merge half done, with the identifiers already moved. It only stopped happening
// when the delete became part of the same statement, which is why this test exists
// here and not in a unit test: the constraint is in the schema.
func TestMergeSurvivesARenditionOnlyCollision(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	name := testSongName(t, "Waiting For Love")

	winner, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name: name, Artists: "Collision Artist", ReleaseDate: text("2019-07-26"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, winner.ID) })

	// Identical in every column unique_release reads, except the rendition the winner
	// is about to inherit.
	loser, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name: name, Artists: "Collision Artist", ReleaseDate: text("2019-07-26"),
		MixName: text("Extended Mix"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}
	// The merge is what removes this row, but a failing merge must not leave it
	// behind: the next run reuses these names and would collide on the insert
	// instead, reporting the wrong fault.
	t.Cleanup(func() { deleteSong(t, loser.ID) })

	if err = q.MergeSongRows(ctx, db.MergeSongRowsParams{
		WinnerID: winner.ID, LoserID: loser.ID,
	}); err != nil {
		t.Fatalf("the merge collided with unique_release instead of replacing the loser: %v", err)
	}

	got, err := q.GetSongByID(ctx, winner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MixName.String != "Extended Mix" {
		t.Errorf("the winner did not take the loser's rendition: %q", got.MixName.String)
	}

	// The loser must be gone, and gone by the merge itself -- no caller deletes it now.
	if _, err = q.GetSongByID(ctx, loser.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("the merged-away row is still present: err = %v", err)
	}
}

// the next sync to overwrite.
func TestLockedFieldsSurviveAMerge(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	winnerName := testSongName(t, "Merge Winner")
	winner, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name: winnerName, Artists: "Merge Artist", ReleaseDate: text("2026-02-01"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, winner.ID) })

	loserName := testSongName(t, "Merge Loser")
	loser, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name: loserName, Artists: "Merge Artist", ReleaseDate: text("2026-02-02"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}

	if _, err = q.DashUpdateSong(ctx, db.DashUpdateSongParams{
		ID: winner.ID, Name: winnerName, Artists: "Merge Artist",
		ReleaseDate: text("2026-02-01"), LockedFields: []string{"release_date"},
	}); err != nil {
		t.Fatalf("DashUpdateSong failed: %v", err)
	}
	if _, err = q.DashUpdateSong(ctx, db.DashUpdateSongParams{
		ID: loser.ID, Name: loserName, Artists: "Merge Artist",
		ReleaseDate: text("2026-02-02"), Lyrics: text("hand entered words"),
		LockedFields: []string{"lyrics"},
	}); err != nil {
		t.Fatalf("DashUpdateSong failed: %v", err)
	}

	if err = q.MergeSongRows(ctx, db.MergeSongRowsParams{
		WinnerID: winner.ID, LoserID: loser.ID,
	}); err != nil {
		t.Fatalf("MergeSongRows failed: %v", err)
	}

	got, err := q.GetSongByID(ctx, winner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Lyrics.String != "hand entered words" {
		t.Errorf("the merge lost the loser's lyrics: %q", got.Lyrics.String)
	}

	want := map[string]bool{"release_date": true, "lyrics": true}
	for _, f := range got.LockedFields {
		delete(want, f)
	}
	if len(want) > 0 {
		t.Errorf("the merge dropped locks %v; locked_fields = %v", want, got.LockedFields)
	}
}

// Deleting a merged-away row used to delete its announcement history with it:
// song_announcements.song_id is ON DELETE CASCADE, and MergeSongRows deletes the loser.
// Repointing first is what keeps the record of already-posted messages, without which
// the refresh loop has no idea they exist.
func TestMergeKeepsAnnouncements(t *testing.T) {
	t.Parallel()

	q := queries(t)
	ctx := context.Background()

	winner, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name: testSongName(t, "Announce Winner"), Artists: "Announce Artist",
		ReleaseDate: text("2026-03-01"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}
	t.Cleanup(func() { deleteSong(t, winner.ID) })

	loser, err := q.InsertRelease(ctx, db.InsertReleaseParams{
		Name: testSongName(t, "Announce Loser"), Artists: "Announce Artist",
		ReleaseDate: text("2026-03-02"),
	})
	if err != nil {
		t.Fatalf("InsertRelease failed: %v", err)
	}

	guildID := int64(690950056202731521)
	messageID := int64(900000000000000000) + loser.ID
	if err = q.InsertSongAnnouncement(ctx, db.InsertSongAnnouncementParams{
		SongID: loser.ID, GuildID: guildID, ChannelID: 1, MessageID: messageID,
		ButtonsKey: "probe",
	}); err != nil {
		t.Fatalf("InsertSongAnnouncement failed: %v", err)
	}

	if _, err = q.DashRepointAnnouncements(ctx, db.DashRepointAnnouncementsParams{
		OldSong: loser.ID, NewSong: winner.ID,
	}); err != nil {
		t.Fatalf("DashRepointAnnouncements failed: %v", err)
	}
	if err = q.MergeSongRows(ctx, db.MergeSongRowsParams{
		WinnerID: winner.ID, LoserID: loser.ID,
	}); err != nil {
		t.Fatalf("MergeSongRows failed: %v", err)
	}

	posted, err := q.DashSongAnnouncements(ctx, winner.ID)
	if err != nil {
		t.Fatalf("DashSongAnnouncements failed: %v", err)
	}
	found := false
	for _, a := range posted {
		if a.MessageID == messageID {
			found = true
		}
	}
	if !found {
		t.Error("the merge lost the announcement the deleted row had posted; " +
			"song_announcements cascades on delete, so it must be repointed first")
	}
}
