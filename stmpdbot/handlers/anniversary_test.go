package handlers

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
)

// The noon anchor is the whole reason <t:...:D> shows the right calendar day. A
// midnight-UTC anchor renders as the previous day for every viewer west of
// Greenwich, which is the kind of bug that looks fine to anyone testing in Europe.
func TestAnniversaryNoonUTC(t *testing.T) {
	released, err := time.Parse(time.DateOnly, "2023-09-01")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	ts := anniversaryNoonUTC(released)
	got := time.Unix(ts, 0).UTC()

	if got.Hour() != 12 {
		t.Errorf("anchor hour = %d, want 12", got.Hour())
	}

	// The calendar date must survive a trip through every populated offset.
	for _, offset := range []int{-12, -8, -5, 0, 1, 5, 9, 11} {
		zone := time.FixedZone("test", offset*3600)
		if local := got.In(zone).Format(time.DateOnly); local != "2023-09-01" {
			t.Errorf("UTC%+d renders %s, want 2023-09-01", offset, local)
		}
	}
}

func TestAnniversaryMonthDays(t *testing.T) {
	tests := []struct {
		name string
		date string
		want []string
	}{
		{"ordinary day", "2026-09-01", []string{"09-01"}},
		{"leap day itself", "2024-02-29", []string{"02-29"}},
		{"1 March in a leap year stays alone", "2024-03-01", []string{"03-01"}},
		{"1 March in a common year adopts 29 Feb", "2026-03-01", []string{"03-01", "02-29"}},
		{"1 March in 2100, not a leap year", "2100-03-01", []string{"03-01", "02-29"}},
		{"1 March in 2000, a leap year", "2000-03-01", []string{"03-01"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := time.Parse(time.DateOnly, tt.date)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			got := anniversaryMonthDays(d)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestAnniversaryLine(t *testing.T) {
	song := db.Song{
		Name:        "Animals",
		Artists:     "Martin Garrix",
		ReleaseDate: pgtype.Text{String: "2013-06-17", Valid: true},
	}

	tests := []struct {
		years    int32
		wantText string
	}{
		{1, "1 year old"},
		{2, "**2 years**"},
		{5, "5 YEARS"},
		{9, "**9 years**"},
		{10, "A DECADE"},
		{13, "**13 years**"},
	}

	for _, tt := range tests {
		line := anniversaryLine(song, tt.years, 1371470400)

		if !strings.Contains(line, tt.wantText) {
			t.Errorf("years=%d: line %q does not contain %q", tt.years, line, tt.wantText)
		}

		// The absolute date survives every branch; it is exact.
		if !strings.Contains(line, "<t:1371470400:D>") {
			t.Errorf("years=%d: line %q is missing the absolute timestamp", tt.years, line)
		}
		// The relative one never appears. It rounds, so beside the literal count it
		// either repeats the number or contradicts it.
		if strings.Contains(line, "<t:1371470400:R>") {
			t.Errorf("years=%d: line %q still renders the rounding relative timestamp", tt.years, line)
		}
		if !strings.Contains(line, "Martin Garrix - Animals") {
			t.Errorf("years=%d: line %q is missing the song", tt.years, line)
		}
	}
}

func TestIsMilestone(t *testing.T) {
	for _, years := range []int32{1, 5, 10} {
		if !isMilestone(years) {
			t.Errorf("isMilestone(%d) = false, want true", years)
		}
	}
	for _, years := range []int32{2, 3, 4, 6, 9, 11, 15, 20} {
		if isMilestone(years) {
			t.Errorf("isMilestone(%d) = true, want false", years)
		}
	}
}

// A crowded day must stay inside Discord's embed-description limit. The catalogue
// has a 16-song day already, and a day that overflows is rejected outright rather
// than truncated, so the "and N more" tail is load-bearing.
func TestBuildSummaryEmbedStaysUnderLimit(t *testing.T) {
	rows := make([]db.GetSongAnniversariesRow, 0, 120)
	for i := 0; i < 120; i++ {
		rows = append(rows, db.GetSongAnniversariesRow{
			Song: db.Song{
				Name:        strings.Repeat("A Very Long Song Title ", 3),
				Artists:     strings.Repeat("An Artist With A Long Name ", 2),
				ReleaseDate: pgtype.Text{String: "2015-12-01", Valid: true},
			},
			YearsOld: 11,
		})
	}

	today, _ := time.Parse(time.DateOnly, "2026-12-01")
	embed := buildSummaryEmbed(rows, today)

	if len(embed.Description) > discordMaxEmbedDescription {
		t.Errorf("description is %d chars, over the %d limit",
			len(embed.Description), discordMaxEmbedDescription)
	}
	if !strings.Contains(embed.Description, "more.") {
		t.Error("overflowing description has no \"and N more\" tail")
	}
}

func TestBuildSummaryEmbedSingleSong(t *testing.T) {
	rows := []db.GetSongAnniversariesRow{{
		Song: db.Song{
			Name:         "The Horizon (With You)",
			Artists:      "DubVision, Nu-La",
			ReleaseDate:  pgtype.Text{String: "2023-09-01", Valid: true},
			ThumbnailUrl: pgtype.Text{String: "https://example.invalid/art.jpg", Valid: true},
		},
		YearsOld: 3,
	}}

	today, _ := time.Parse(time.DateOnly, "2026-09-01")
	embed := buildSummaryEmbed(rows, today)

	if embed.Title != "On This Day - 1 September" {
		t.Errorf("title = %q", embed.Title)
	}
	if !strings.Contains(embed.Description, "**3 years**") {
		t.Errorf("description = %q, want a 3 years line", embed.Description)
	}
	if embed.Thumbnail == nil || embed.Thumbnail.URL != "https://example.invalid/art.jpg" {
		t.Error("thumbnail was not carried over from the song")
	}
	if embed.Footer == nil || !strings.Contains(embed.Footer.Text, "1 song") {
		t.Error("footer should say \"1 song\", not \"1 songs\"")
	}
}
