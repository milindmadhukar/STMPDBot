package utils

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
)

func sameDayRow(id int64, name, artists, mix, date string, beatportID int32, slug string) db.GetAllSongsForMatchingRow {
	return db.GetAllSongsForMatchingRow{
		ID: id, Name: name, Artists: artists, MixName: text(mix),
		ReleaseDate: text(date), StmpdSlug: text(slug),
		BeatportID: pgtype.Int4{Int32: beatportID, Valid: beatportID != 0},
		MatchKey:   text(MatchKey(name, "", mix, artists)),
		BaseKey:    text(BaseKey(name, artists)),
	}
}

// Both of these were announced twice in November 2026: once from the STMPD dataset,
// once from beatport, because the dataset's credit did not match beatport's exactly.
func TestSameReleaseDayForgivesAMangledCredit(t *testing.T) {
	cases := []struct {
		name   string
		stored db.GetAllSongsForMatchingRow
		query  SongQuery
	}{
		{
			name: "dataset swapped title and artists, beatport arrives second",
			stored: sameDayRow(16434, "Firebeatz & Skytech", "Break It Down", "", "2026-11-13", 0,
				"break-it-down-firebeatz-and-skytech-undefined-2026-11-13"),
			query: SongQuery{Title: "Break It Down", MixName: "Extended Mix", Artists: "Skytech, Firebeatz",
				BeatportID: pgtype.Int4{Int32: 31251094, Valid: true}, ReleaseDate: "2026-11-13"},
		},
		{
			name:   "dataset swapped title and artists, dataset arrives second",
			stored: sameDayRow(16435, "Break It Down", "Skytech, Firebeatz", "Extended Mix", "2026-11-13", 31251094, ""),
			query: SongQuery{Title: "Firebeatz & Skytech", Artists: "Break It Down",
				StmpdSlug: "break-it-down-firebeatz-and-skytech-undefined-2026-11-13", ReleaseDate: "2026-11-13"},
		},
		{
			name: "dataset misspells an artist, beatport arrives second",
			stored: sameDayRow(16436, "Bring It Back", "Lucas & Steven, Sick Individuals", "", "2026-11-20", 0,
				"lucas-and-steven-sick-individuals-bring-it-back-undefined-2026-11-20"),
			query: SongQuery{Title: "Bring It Back", MixName: "Extended Mix", Artists: "SICK INDIVIDUALS, Lucas & Steve",
				BeatportID: pgtype.Int4{Int32: 31317269, Valid: true}, ReleaseDate: "2026-11-20"},
		},
		{
			name:   "dataset misspells an artist, dataset arrives second",
			stored: sameDayRow(16438, "Bring It Back", "SICK INDIVIDUALS, Lucas & Steve", "Extended Mix", "2026-11-20", 31317269, ""),
			query: SongQuery{Title: "Bring It Back", Artists: "Lucas & Steven, Sick Individuals",
				StmpdSlug: "lucas-and-steven-sick-individuals-bring-it-back-undefined-2026-11-20", ReleaseDate: "2026-11-20"},
		},
		{
			name:   "one side credits an extra artist",
			stored: sameDayRow(1, "Summer Days", "Martin Garrix", "", "2026-11-20", 0, "martin-garrix-summer-days"),
			query: SongQuery{Title: "Summer Days", Artists: "Martin Garrix, Macklemore",
				BeatportID: pgtype.Int4{Int32: 1, Valid: true}, ReleaseDate: "2026-11-20"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ix := NewSongIndex([]db.GetAllSongsForMatchingRow{c.stored})
			got, tier := ix.Lookup(c.query)
			if got == nil || got.ID != c.stored.ID {
				t.Fatalf("got %v at tier %q; want #%d", got, tier, c.stored.ID)
			}
			if tier != MatchSameReleaseDay {
				t.Errorf("tier = %q; want %q", tier, MatchSameReleaseDay)
			}
		})
	}
}

func TestSameReleaseDayStillSeparatesDifferentRecordings(t *testing.T) {
	stored := []db.GetAllSongsForMatchingRow{
		sameDayRow(1, "Bring It Back", "Lucas & Steven, Sick Individuals", "", "2026-11-20", 0, "lucas-and-steven-sick-individuals-bring-it-back"),
	}
	cases := []struct {
		name  string
		query SongQuery
	}{
		{"a different day", SongQuery{Title: "Bring It Back", Artists: "Lucas & Steve, Sick Individuals",
			BeatportID: pgtype.Int4{Int32: 7, Valid: true}, ReleaseDate: "2026-11-27"}},
		{"a named remix the same day", SongQuery{Title: "Bring It Back", MixName: "Mesto Remix",
			Artists: "Lucas & Steve, Sick Individuals, Mesto", BeatportID: pgtype.Int4{Int32: 7, Valid: true}, ReleaseDate: "2026-11-20"}},
		{"a different song by the same act the same day", SongQuery{Title: "Bring It Home",
			Artists: "Lucas & Steve, Sick Individuals", BeatportID: pgtype.Int4{Int32: 7, Valid: true}, ReleaseDate: "2026-11-20"}},
		{"the same title by an unrelated act the same day", SongQuery{Title: "Bring It Back",
			Artists: "TV Noise", BeatportID: pgtype.Int4{Int32: 7, Valid: true}, ReleaseDate: "2026-11-20"}},
		{"no release date", SongQuery{Title: "Bring It Back", Artists: "Lucas & Steve, Sick Individuals",
			BeatportID: pgtype.Int4{Int32: 7, Valid: true}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ix := NewSongIndex(stored)
			if got, tier := ix.Lookup(c.query); got != nil {
				t.Errorf("matched #%d at tier %q; want no match", got.ID, tier)
			}
		})
	}
}
