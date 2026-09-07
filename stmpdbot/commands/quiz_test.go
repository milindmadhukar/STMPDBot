package commands

// In-package: filterValidLines and selectLyricLines are unexported.

import (
	"slices"
	"strings"
	"testing"
)

// one wraps a single title as the answer set, for the cases that predate a song
// having more than one accepted spelling.
func one(title string) []string { return []string{title} }

func TestFilterValidLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		lines   []string
		answers []string
		want    []string
	}{
		{
			name:    "keeps lines that do not give the answer away",
			lines:   []string{"we are the people", "that you'll never get the best of"},
			answers: one("Animals"),
			want:    []string{"we are the people", "that you'll never get the best of"},
		},
		{
			name:    "drops a line containing the song name",
			lines:   []string{"we are the animals", "keep this one"},
			answers: one("Animals"),
			want:    []string{"keep this one"},
		},
		{
			name:    "the song name match is case insensitive",
			lines:   []string{"WE ARE THE ANIMALS", "keep this one"},
			answers: one("animals"),
			want:    []string{"keep this one"},
		},
		{
			name:    "drops lines under five characters",
			lines:   []string{"hey", "oh", "a longer line"},
			answers: one("Animals"),
			want:    []string{"a longer line"},
		},
		{
			name:    "trims surrounding whitespace",
			lines:   []string{"   padded line   "},
			answers: one("Animals"),
			want:    []string{"padded line"},
		},
		{
			name:    "a line that is only whitespace is dropped",
			lines:   []string{"        ", "a longer line"},
			answers: one("Animals"),
			want:    []string{"a longer line"},
		},
		{
			name:    "exactly five characters is kept",
			lines:   []string{"12345"},
			answers: one("Animals"),
			want:    []string{"12345"},
		},
		{
			name:    "four characters is dropped",
			lines:   []string{"1234"},
			answers: one("Animals"),
		},
		{
			// The Breach round: the stored name never appears in a lyric because no
			// lyric contains parentheses, so only filtering on the subtitle catches it.
			name:    "drops a line containing the subtitle",
			lines:   []string{"You'll never walk alone", "The dead of the night, out in the cold"},
			answers: []string{"Breach (Walk Alone)", "Breach", "Walk Alone"},
			want:    []string{"The dead of the night, out in the cold"},
		},
		{
			// Word boundaries, not substrings: a title of "Not" must not hide every
			// line containing "nothing".
			name:    "a title inside a longer word is not a giveaway",
			lines:   []string{"there is nothing left to say"},
			answers: []string{"It's Alright (Not)", "It's Alright", "Not"},
			want:    []string{"there is nothing left to say"},
		},
		{
			// The Voodoo round: a hand-pasted synced lyric left "[02:14.11]" as its own
			// line, an instrumental-break marker with no lyric text after the tag. At
			// ten characters it beat the length filter and became the only clue shown.
			name:    "a bare LRC timestamp is dropped, not judged on its own length",
			lines:   []string{"[02:14.11]", "[00:12.42] a proper lyric line"},
			answers: one("Animals"),
			want:    []string{"a proper lyric line"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := filterValidLines(tt.lines, tt.answers, 1)
			if !slices.Equal(got, tt.want) {
				t.Errorf("filterValidLines() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Returns nil rather than an empty slice, so callers must test with len().
func TestFilterValidLines_NoMatchesReturnsNil(t *testing.T) {
	t.Parallel()

	for _, lines := range [][]string{nil, {}, {"hey", "yo"}} {
		if got := filterValidLines(lines, one("Animals"), 1); got != nil {
			t.Errorf("filterValidLines(%v) = %v, want nil", lines, got)
		}
	}
}

// An empty title used to filter every line out, because strings.Contains(x, "") is
// always true. Empty forms are now skipped rather than matched.
func TestFilterValidLines_EmptyTitleKeepsEverything(t *testing.T) {
	t.Parallel()

	lines := []string{"a perfectly good line", "another good line"}

	if got := filterValidLines(lines, one(""), 1); !slices.Equal(got, lines) {
		t.Errorf("filterValidLines(%v, \"\") = %v, want every line kept", lines, got)
	}
}

// The strictest tier can hide the whole song when an accepted form is an ordinary
// word. Weakening the filter beats showing the player nothing.
func TestFilterValidLines_WeakensUntilEnoughLinesSurvive(t *testing.T) {
	t.Parallel()

	// Every line names the subtitle, so tier one leaves nothing.
	lines := []string{
		"tasty is all I ever wanted",
		"and tasty is all I need",
		"nothing but tasty tonight",
	}
	answers := []string{"Melt (Tasty)", "Melt", "Tasty"}

	got := filterValidLines(lines, answers, 2)
	if len(got) != 3 {
		t.Fatalf("got %d lines, want all 3 after the filter weakened: %v", len(got), got)
	}

	// The stored name and the base title are still hidden at the tier that won, so a
	// line naming "Melt" is dropped while the subtitle-only lines survive.
	withBase := append(slices.Clone(lines), "I watch it melt away")
	if got := filterValidLines(withBase, answers, 2); slices.Contains(got, "I watch it melt away") {
		t.Errorf("the base title should still be hidden at the second tier: %v", got)
	}
}

func TestSelectLyricLines_CountPerDifficulty(t *testing.T) {
	t.Parallel()

	lines := []string{"one", "two", "three", "four", "five", "six", "seven", "eight"}

	tests := []struct {
		difficulty string
		want       int
	}{
		{"easy", 4},
		{"medium", 3},
		{"hard", 2},
		{"extreme", 1},
		{"unrecognised", 4}, // falls back to the easy count
		{"", 4},
	}

	for _, tt := range tests {
		t.Run(tt.difficulty, func(t *testing.T) {
			t.Parallel()

			got := selectLyricLines(lines, tt.difficulty)
			if len(got) != tt.want {
				t.Errorf("selectLyricLines(_, %q) returned %d lines, want %d",
					tt.difficulty, len(got), tt.want)
			}
		})
	}
}

// filterValidLines and selectLyricLines have to agree about how many lines a round
// needs, or the filter weakens for a target the round never wanted.
func TestLineCountForMatchesSelection(t *testing.T) {
	t.Parallel()

	lines := []string{"one", "two", "three", "four", "five", "six", "seven", "eight"}

	for _, difficulty := range []string{"easy", "medium", "hard", "extreme", "unrecognised"} {
		if got, want := len(selectLyricLines(lines, difficulty)), lineCountFor(difficulty); got != want {
			t.Errorf("difficulty %q selects %d lines but lineCountFor says %d", difficulty, got, want)
		}
	}
}

func TestSelectLyricLines_EdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("no lines gives no lines", func(t *testing.T) {
		t.Parallel()

		if got := selectLyricLines(nil, "easy"); len(got) != 0 {
			t.Errorf("got %d lines from an empty input, want 0", len(got))
		}
	})

	t.Run("the count is clamped to what is available", func(t *testing.T) {
		t.Parallel()

		lines := []string{"only", "two"}
		got := selectLyricLines(lines, "easy") // easy wants 4
		if len(got) != 2 {
			t.Errorf("got %d lines, want all 2 that were available", len(got))
		}
	})

	t.Run("a single line", func(t *testing.T) {
		t.Parallel()

		got := selectLyricLines([]string{"only one"}, "easy")
		if len(got) != 1 || got[0] != "only one" {
			t.Errorf("got %v, want the single available line", got)
		}
	})
}

// The selection window is random, so assert the invariants instead of an exact
// result: it must always be a contiguous run taken from the input.
func TestSelectLyricLines_IsAlwaysAContiguousWindow(t *testing.T) {
	t.Parallel()

	lines := []string{"one", "two", "three", "four", "five", "six", "seven", "eight"}

	for _, difficulty := range []string{"easy", "medium", "hard", "extreme"} {
		for range 200 {
			got := selectLyricLines(lines, difficulty)
			if len(got) == 0 {
				t.Fatalf("difficulty %q returned no lines", difficulty)
			}

			start := slices.Index(lines, got[0])
			if start < 0 {
				t.Fatalf("difficulty %q returned %q, which is not in the input",
					difficulty, got[0])
			}
			if start+len(got) > len(lines) {
				t.Fatalf("difficulty %q returned a window running past the end of the input",
					difficulty)
			}
			if !slices.Equal(got, lines[start:start+len(got)]) {
				t.Fatalf("difficulty %q returned %v, which is not contiguous in the input",
					difficulty, got)
			}
		}
	}
}

// The window is a subslice of the caller's slice, not a copy, so writing to it
// writes through to the input.
func TestSelectLyricLines_AliasesTheInputSlice(t *testing.T) {
	t.Parallel()

	lines := []string{"one", "two", "three", "four"}
	got := selectLyricLines(lines, "easy") // takes all four

	got[0] = "mutated"

	if !slices.Contains(lines, "mutated") {
		t.Error("the returned window no longer aliases the input; if that was " +
			"deliberate, delete this test")
	}
}

// The two functions run back to back in the handler: filter the lyrics, then
// pick a window from what survives.
func TestQuizLinePipeline(t *testing.T) {
	t.Parallel()

	lyrics := strings.Split(
		"We are the animals\n"+
			"hey\n"+
			"Never gonna give you up\n"+
			"   \n"+
			"Never gonna let you down\n"+
			"Never gonna run around",
		"\n")

	valid := filterValidLines(lyrics, one("Animals"), lineCountFor("hard"))
	if len(valid) != 3 {
		t.Fatalf("got %d valid lines, want 3: %v", len(valid), valid)
	}

	for _, line := range valid {
		if strings.Contains(strings.ToLower(line), "animals") {
			t.Errorf("line %q gives the answer away", line)
		}
		if len(line) < 5 {
			t.Errorf("line %q is too short to have survived", line)
		}
	}

	if got := selectLyricLines(valid, "hard"); len(got) != 2 {
		t.Errorf("got %d lines for a hard quiz, want 2", len(got))
	}
}
