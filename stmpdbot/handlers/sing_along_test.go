package handlers

import (
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// TestSingAlongLocalDate pins the one thing about the daily key that is easy to get
// wrong: the date is the guild's OWN calendar day, stamped at midnight UTC so pgx
// does not shift it on the way into a DATE column.
func TestSingAlongLocalDate(t *testing.T) {
	for _, offset := range []int{-12, -8, -5, 0, 1, 5, 8, 13} {
		loc := time.FixedZone("test", offset*3600)

		// Late evening and early morning are where a naive UTC key disagrees with
		// the local calendar day.
		for _, hour := range []int{0, 9, 23} {
			local := time.Date(2026, time.March, 14, hour, 30, 0, 0, loc)

			got := singAlongLocalDate(local)
			if !got.Valid {
				t.Fatalf("offset %+d hour %d: date is not valid", offset, hour)
			}

			want := time.Date(2026, time.March, 14, 0, 0, 0, 0, time.UTC)
			if !got.Time.Equal(want) {
				t.Errorf("offset %+d hour %d: got %s, want %s",
					offset, hour, got.Time.Format(time.RFC3339), want.Format(time.RFC3339))
			}
		}
	}
}

// TestSingAlongPermissionsAreComplete guards against a permission being dropped from
// the up-front gate and only being discovered as a failed API call mid-round.
func TestSingAlongPermissionsAreComplete(t *testing.T) {
	want := map[string]bool{
		"View Channel":         true,
		"Send Messages":        true,
		"Manage Messages":      true,
		"Add Reactions":        true,
		"Read Message History": true,
		"Manage Channels":      true,
	}

	if len(singAlongPermissions) != len(want) {
		t.Fatalf("got %d permissions, want %d", len(singAlongPermissions), len(want))
	}

	for _, permission := range singAlongPermissions {
		if !want[permission.Name] {
			t.Errorf("unexpected permission %q in the gate", permission.Name)
		}
		if permission.Flag == 0 {
			t.Errorf("%q has no permission bit", permission.Name)
		}
	}
}

// One message over Discord's two-week line rejects the entire bulk call, so the
// split has to happen before the call rather than in response to its failure --
// otherwise a channel with any history at all deletes a hundred messages one at a
// time when it could have made a single request.
func TestSplitOnBulkDeleteAge(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)

	// Discord's epoch, which is what snowflake.ID.Time() decodes against.
	at := func(d time.Duration) snowflake.ID {
		return snowflake.New(now.Add(-d))
	}

	messages := []discord.Message{
		{ID: at(time.Minute)},
		{ID: at(24 * time.Hour)},
		{ID: at(13 * 24 * time.Hour)},
		{ID: at(20 * 24 * time.Hour)},
		{ID: at(400 * 24 * time.Hour)},
	}

	fresh, stale := splitOnBulkDeleteAge(messages, now)

	if len(fresh) != 3 {
		t.Errorf("got %d bulk-deletable messages, want 3", len(fresh))
	}
	if len(stale) != 2 {
		t.Errorf("got %d one-at-a-time messages, want 2", len(stale))
	}

	// Every message must end up in exactly one of the two buckets, or the wipe
	// silently leaves some behind and re-reads the same page until the cap.
	if len(fresh)+len(stale) != len(messages) {
		t.Errorf("%d messages went in, %d came out", len(messages), len(fresh)+len(stale))
	}
}

// The margin exists because Discord judges the boundary on its clock, not ours.
func TestBulkDeleteMaxAgeLeavesAMargin(t *testing.T) {
	const discordLimit = 14 * 24 * time.Hour
	if bulkDeleteMaxAge >= discordLimit {
		t.Errorf("bulkDeleteMaxAge = %s, must be under Discord's %s", bulkDeleteMaxAge, discordLimit)
	}
	if discordLimit-bulkDeleteMaxAge > 24*time.Hour {
		t.Errorf("bulkDeleteMaxAge = %s gives up more than a day of the bulk window", bulkDeleteMaxAge)
	}
}

// Discord decides whether to show an attachment inline from its extension, so an
// artwork URL that carries none still has to be uploaded under a name that has one.
func TestCoverFilename(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{"jpg", "https://cdn.example.com/art/abc123.jpg", "cover.jpg"},
		{"png", "https://cdn.example.com/art/abc123.png", "cover.png"},
		{"webp", "https://cdn.example.com/art/abc.WEBP", "cover.webp"},
		{"no extension", "https://is1-ssl.mzstatic.com/image/thumb/abc/600x600bb", "cover.jpg"},
		{"query string", "https://cdn.example.com/a.png?width=600", "cover.png"},
		{"not an image extension", "https://cdn.example.com/art/abc.aspx", "cover.jpg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := coverFilename(tc.url); got != tc.want {
				t.Errorf("coverFilename(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}
