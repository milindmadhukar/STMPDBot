package handlers

import (
	"testing"
	"time"
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
