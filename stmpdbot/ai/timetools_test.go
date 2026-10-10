package ai

import (
	"strings"
	"testing"
	"time"
)

func TestResolveTimestampNextOccurrence(t *testing.T) {
	cest := time.FixedZone("CEST", 2*3600)
	now := time.Date(2026, 10, 11, 21, 0, 0, 0, cest) // 9pm CEST

	cases := []struct {
		clock, date, zone string
		want              time.Time
	}{
		{"8pm", "", "CEST", time.Date(2026, 10, 12, 20, 0, 0, 0, cest)},    // passed -> tomorrow
		{"22:30", "", "cest", time.Date(2026, 10, 11, 22, 30, 0, 0, cest)}, // still ahead -> today
		{"9 PM", "", "CEST", time.Date(2026, 10, 12, 21, 0, 0, 0, cest)},   // exactly now -> tomorrow
		{"20:00", "2026-12-31", "UTC+2", time.Date(2026, 12, 31, 20, 0, 0, 0, cest)},
		{"", "2027-01-01", "UTC", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"8:30pm", "", "IST", time.Date(2026, 10, 12, 20, 30, 0, 0, time.FixedZone("IST", 5*3600+1800))},
	}
	for _, c := range cases {
		got, err := resolveTimestamp(now, c.clock, c.date, c.zone)
		if err != nil {
			t.Fatalf("%v: %v", c, err)
		}
		if !got.Equal(c.want) {
			t.Errorf("%v: got %v want %v", c, got, c.want)
		}
	}

	ams, err := resolveZone("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := resolveTimestamp(now, "20:00", "2026-12-01", "Europe/Amsterdam")
	if want := time.Date(2026, 12, 1, 20, 0, 0, 0, ams); !got.Equal(want) || got.Unix() != want.Unix() {
		t.Errorf("iana: got %v", got)
	}

	if _, err := resolveTimestamp(now, "8pm", "", "Mars/Olympus"); err == nil {
		t.Error("want error for unknown zone")
	}
}

func TestDiscordTimestampTag(t *testing.T) {
	now := time.Date(2026, 10, 11, 19, 0, 0, 0, time.UTC)
	out, err := dispatchTimeToolAt(now, "discord_timestamp", `{"time":"20:00","timezone":"UTC","style":"t"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := `"tag":"<t:1791748800:t>"`
	if !strings.Contains(out, want) {
		t.Errorf("got %s, want %s", out, want)
	}
}
