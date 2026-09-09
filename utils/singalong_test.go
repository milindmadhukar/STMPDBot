package utils

import (
	"reflect"
	"sync"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

func TestLyricLines(t *testing.T) {
	tests := []struct {
		name   string
		lyrics string
		want   []string
	}{
		{
			name:   "keeps the order of the song",
			lyrics: "first line here\nsecond line here\nthird line here",
			want:   []string{"first line here", "second line here", "third line here"},
		},
		{
			name:   "drops blank and whitespace-only lines",
			lyrics: "a proper lyric line\n\n   \nanother proper line",
			want:   []string{"a proper lyric line", "another proper line"},
		},
		{
			name:   "drops lines under five characters",
			lyrics: "hey\noh\na longer line",
			want:   []string{"a longer line"},
		},
		{
			name:   "strips a run of LRC timestamps",
			lyrics: "[00:12.00][00:45.00]a proper lyric line",
			want:   []string{"a proper lyric line"},
		},
		{
			// The reason CleanLyricLine measures what is LEFT: a bare tag is longer
			// than the length floor and would otherwise survive as a lyric.
			name:   "a bare timestamp is not a line",
			lyrics: "[02:14.11]\nan actual lyric line",
			want:   []string{"an actual lyric line"},
		},
		{
			name:   "trims surrounding whitespace",
			lyrics: "   padded line   ",
			want:   []string{"padded line"},
		},
		{
			name:   "carriage returns do not become part of the line",
			lyrics: "first line here\r\nsecond line here",
			want:   []string{"first line here", "second line here"},
		},
		{
			name:   "nothing singable returns nil",
			lyrics: "\n  \nhey",
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LyricLines(tt.lyrics); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("LyricLines(%q) = %q, want %q", tt.lyrics, got, tt.want)
			}
		})
	}
}

func TestLineMatches(t *testing.T) {
	const line = "Ooh, I'm scared of the dark without you here"

	tests := []struct {
		name    string
		attempt string
		want    bool
	}{
		{"exact", line, true},
		{"case does not matter", "OOH IM SCARED OF THE DARK WITHOUT YOU HERE", true},
		{"punctuation does not matter", "ooh im scared of the dark without you here", true},
		{"spacing does not matter", "Ooh,   I'm scared of the dark   without you here", true},
		{"one typo is forgiven", "Ooh, I'm scared of the drak without you here", true},

		// Nobody types a lyric back exactly. These are what people actually send,
		// and every one of them is somebody who plainly knows the line.
		{"a dropped opening filler", "im scared of the dark without you here", true},
		{"text speak", "ooh im scared of the dark without u here", true},
		{"one word remembered wrong", "Ooh, I'm afraid of the dark without you here", true},
		{"a dropped word in the middle", "Ooh, I'm scared of dark without you here", true},

		{"a single word is not the line", "scared", false},
		{"a different line is not the line", "And I'm walking on a wire in the pouring rain", false},
		{"empty is not the line", "   ", false},
		{"a wall of text is not the line", line + " " + line + " " + line + " " + line + " " + line, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LineMatches(line, tt.attempt); got != tt.want {
				t.Errorf("LineMatches(%q, %q) = %v, want %v", line, tt.attempt, got, tt.want)
			}
		})
	}
}

func TestRoundJudge(t *testing.T) {
	round := SingAlongRound{
		Lines: []string{
			"We are the ones who will never be beaten",
			"Together we stand and divided we fall",
			"Every night we are dancing till the morning",
		},
		Cursor: 1,
	}

	tests := []struct {
		name    string
		message string
		want    SingAlongVerdict
	}{
		{"the line being waited for", "Together we stand and divided we fall", SingAlongCorrect},
		{"the line already on screen", "We are the ones who will never be beaten", SingAlongEcho},
		{"a line from later in the song", "Every night we are dancing till the morning", SingAlongWrong},
		{"ordinary chat", "yo who else is here", SingAlongWrong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := round.Judge(tt.message); got != tt.want {
				t.Errorf("Judge(%q) = %v, want %v", tt.message, got, tt.want)
			}
		})
	}

	t.Run("a completed round judges nothing wrong", func(t *testing.T) {
		done := round
		done.Complete = true
		if got := done.Judge("anything at all"); got != SingAlongEcho {
			t.Errorf("Judge on a completed round = %v, want SingAlongEcho", got)
		}
	})

	t.Run("a chorus that repeats a line still counts as an answer", func(t *testing.T) {
		repeated := SingAlongRound{
			Lines:  []string{"Never gonna give you up", "Never gonna give you up"},
			Cursor: 1,
		}
		if got := repeated.Judge("Never gonna give you up"); got != SingAlongCorrect {
			t.Errorf("Judge on a repeated line = %v, want SingAlongCorrect", got)
		}
	})
}

func TestStateAdvanceHasOneWinner(t *testing.T) {
	const channelID = snowflake.ID(1234)

	state := NewSingAlongState()
	state.Set(SingAlongRound{
		ChannelID: channelID,
		Lines:     []string{"one line here", "two lines here", "three lines here"},
		Cursor:    1,
	})

	// Two members answering the same line in the same instant. Exactly one of them
	// may be paid, which is the whole reason Advance is a compare-and-swap.
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		start = make(chan struct{})
	)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, won := state.Advance(channelID, 1); won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Errorf("got %d winners for one line, want 1", wins)
	}

	round, _ := state.Round(channelID)
	if round.Cursor != 2 {
		t.Errorf("cursor = %d, want 2", round.Cursor)
	}
}

func TestStateAdvanceCompletesAndLatches(t *testing.T) {
	const channelID = snowflake.ID(99)

	state := NewSingAlongState()
	state.Set(SingAlongRound{
		ChannelID: channelID,
		Lines:     []string{"one line here", "two lines here"},
		Cursor:    1,
	})

	round, won := state.Advance(channelID, 1)
	if !won {
		t.Fatal("the last line should have been won")
	}
	if !round.Complete {
		t.Error("running off the end of the song should complete the round")
	}

	if _, won := state.Advance(channelID, 2); won {
		t.Error("a completed round should not advance again")
	}
	if _, won := state.Advance(channelID, 1); won {
		t.Error("a stale cursor should not advance")
	}
}

func TestStateRetainDropsUnconfiguredChannels(t *testing.T) {
	state := NewSingAlongState()
	state.Set(SingAlongRound{ChannelID: 1, Lines: []string{"a line here"}})
	state.Set(SingAlongRound{ChannelID: 2, Lines: []string{"a line here"}})

	state.Retain([]snowflake.ID{1})

	if _, ok := state.Round(1); !ok {
		t.Error("a still-configured channel was dropped")
	}
	// Without this a channel un-set in the dashboard would go on deleting members'
	// messages until the bot was restarted.
	if _, ok := state.Round(2); ok {
		t.Error("an un-configured channel kept its round")
	}
}

// The give comes from Judge picking the CLOSEST line rather than clearing a bar, so
// these are the invariants that matter -- not how any single pair scores.
func TestJudgeIsDiscriminative(t *testing.T) {
	round := SingAlongRound{
		Lines: []string{
			"We are the ones who will never be beaten",
			"Together we stand and divided we fall",
			"Every night we are dancing till the morning",
			"Scared of the dark",
		},
		Cursor: 1,
	}

	t.Run("a badly mangled answer still counts", func(t *testing.T) {
		// A whole word gone and another wrong. Nowhere near the line, but nearer it
		// than anything else in the song, which is what being an attempt means.
		for _, attempt := range []string{
			"together we stand and divided we all",
			"together we stand divided we fall",
			"togather we stand and divided we fal",
		} {
			if got := round.Judge(attempt); got != SingAlongCorrect {
				t.Errorf("Judge(%q) = %v, want SingAlongCorrect", attempt, got)
			}
		}
	})

	t.Run("a different line of the song is not the answer", func(t *testing.T) {
		// Clears the floor comfortably -- it is a real lyric -- but belongs to a
		// line the round is not waiting for.
		if got := round.Judge("Every night we are dancing till the morning"); got != SingAlongWrong {
			t.Errorf("jumping ahead = %v, want SingAlongWrong", got)
		}
	})

	t.Run("the line already on screen is an echo", func(t *testing.T) {
		if got := round.Judge("We are the ones who will never be beaten"); got != SingAlongEcho {
			t.Errorf("repeating the previous line = %v, want SingAlongEcho", got)
		}
	})

	t.Run("ordinary chat is not an attempt", func(t *testing.T) {
		// The floor's only job. None of these is close to any line in the song.
		for _, chat := range []string{
			"yo who else is here", "this song is so good", "brb", "gm everyone",
			"anyone up for a game", "garrix is the goat",
		} {
			if got := round.Judge(chat); got != SingAlongWrong {
				t.Errorf("Judge(%q) = %v, want SingAlongWrong", chat, got)
			}
		}
	})
}

// The floor exists to keep chat out, and nothing else -- Judge's best-match rule is
// what decides whether an attempt is the right line. Measured over 2055 catalogue
// lines, the highest an ordinary channel message scored against the nearest line of
// a song was 0.571, which is what the floor sits above.
func TestSingAlongThresholdStaysInItsBand(t *testing.T) {
	if SingAlongThreshold <= 0.571 {
		t.Errorf("SingAlongThreshold = %.2f; at or below 0.571 ordinary chat starts "+
			"registering as an attempt", SingAlongThreshold)
	}
	if SingAlongThreshold > 0.70 {
		t.Errorf("SingAlongThreshold = %.2f; above 0.70 it starts rejecting attempts "+
			"the best-match rule would have identified correctly", SingAlongThreshold)
	}
}
