package utils

import (
	"regexp"
	"strings"
	"sync"

	"github.com/disgoorg/snowflake/v2"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
)

// SingAlongThreshold is how close a message has to be to the line it is answering.
//
// Nobody types a lyric back exactly, so this is measured rather than guessed. Across
// 1460 real lines from the catalogue, comparing each line against a version of
// itself with one whole word misheard:
//
//	threshold   misheard word accepted   adjacent-line false positives
//	0.85        4.4%                     7.81%
//	0.75        48.8%                    8.57%
//	0.70        65.8%                    8.84%
//	0.60        91.0%                    11.58%
//
// The first column is why 0.85 was wrong: it demanded near-perfect recall and threw
// away all but a twentieth of the attempts where somebody remembered the line but
// not one word of it. A single typo passes at any of these.
//
// The second column is why loosening is nearly free. Those false positives are
// mostly not near misses at all -- they are lines a song repeats VERBATIM, where
// adjacent pairs score exactly 1.0, and no threshold can separate them because there
// is nothing to separate. That floor sits at 7.8% and moving from 0.85 to 0.70 buys
// fifteen times the tolerance for one percentage point of it.
//
// 0.70 is also what the bot used in its Python days -- cogs/fun.py matched guesses
// with difflib at 0.7 -- so this lands back on a number the community was already
// playing against, from the other direction.
//
// It stays well above QuizThreshold's 0.6 because that compares a guess against a
// song TITLE, where a couple of characters are a large fraction of the string.
const SingAlongThreshold = 0.70

// singAlongMaxLengthRatio rejects a message far longer than the line it claims to
// be, before the edit-distance matrix runs.
//
// SimilarityScore normalises by the longer string, so a wall of pasted text already
// scores near zero and would be rejected anyway. This is only about not building an
// n-by-m matrix for a two thousand character paste on the gateway's goroutine.
const singAlongMaxLengthRatio = 4

// minLyricLineLength is the shortest run of characters that can be a line worth
// singing or quizzing on. Anything under it is a stray bracket or an ad-lib.
const minLyricLineLength = 5

// lrcTimestampPrefix matches leading LRC-style timestamp tags such as "[02:14.11]",
// including the run of several a synced line can carry ("[00:12.00][00:45.00]text").
//
// songs.lyrics is meant to hold plain, untimed lyrics -- see lrclib.go -- but a few
// rows were pasted by hand from a synced source and kept their tags. A line that is
// nothing but a tag, like an instrumental break marker, has no lyric content once
// the tag is gone and must not be judged on the length of the timestamp instead.
var lrcTimestampPrefix = regexp.MustCompile(`^(?:\[\d{1,2}:\d{2}(?:\.\d{1,3})?\]\s*)+`)

// CleanLyricLine trims one lyric line, strips any stray LRC timestamp tags, and
// reports whether what is left is long enough to be worth singing or quizzing on.
//
// The single home for that rule. The quiz applies it while filtering giveaway lines
// and the sing-along applies it to the whole song up front, and the two must agree
// about what counts as a line -- otherwise a round could be built on a line the quiz
// considers noise, or vice versa.
func CleanLyricLine(line string) (string, bool) {
	content := strings.TrimSpace(lrcTimestampPrefix.ReplaceAllString(strings.TrimSpace(line), ""))
	if len(content) < minLyricLineLength {
		return "", false
	}
	return content, true
}

// LyricLines splits plain lyrics into the cleaned, ordered lines a round is played
// on. Order is preserved, because the sing-along walks the song in sequence -- which
// is the one property the quiz never needed and this depends on entirely.
func LyricLines(lyrics string) []string {
	var kept []string
	for _, line := range strings.Split(lyrics, "\n") {
		if content, ok := CleanLyricLine(line); ok {
			kept = append(kept, content)
		}
	}
	return kept
}

// lyricLineScore is how well a message answers one lyric line, or 0 when the message
// is too far off in length to be worth measuring.
//
// SimilarityScore already lowercases and drops spaces and punctuation, so "Ooh, I'm
// scared of the dark!" answers "ooh im scared of the dark". What it does not do is
// forgive extra words, which is intended: singing the line means singing the line.
func lyricLineScore(want, got string) float64 {
	w, g := preprocessString(want), preprocessString(got)
	if len(w) == 0 || len(g) == 0 {
		return 0
	}
	if len(g) > len(w)*singAlongMaxLengthRatio {
		return 0
	}
	return SimilarityScore(want, got)
}

// LineMatches reports whether a message is the lyric line being waited for.
func LineMatches(want, got string) bool {
	return lyricLineScore(want, strings.TrimSpace(got)) >= SingAlongThreshold
}

// SingAlongVerdict is what a round makes of one message.
type SingAlongVerdict int

const (
	// SingAlongWrong is a message that is not the line being waited for. It is
	// deleted, and Discord's slowmode makes the member wait before trying again.
	SingAlongWrong SingAlongVerdict = iota
	// SingAlongCorrect wins the line: a tick, the coins, and the song moves on.
	SingAlongCorrect
	// SingAlongEcho is somebody repeating the line already on screen. It is neither
	// rewarded nor deleted -- deleting a member for reading back the line the bot
	// just posted is the first complaint this feature would ever get.
	SingAlongEcho
)

// SingAlongRound is one guild's live round, as the message listener needs to see it.
//
// It is a value, not a pointer, so a caller cannot mutate the registry's copy by
// accident; every change goes through SingAlongState.
type SingAlongRound struct {
	GuildID   snowflake.ID
	ChannelID snowflake.ID
	// Song is the record being sung. HasSong is false when the row was merged away
	// mid-round -- see the sing_along_rounds migration -- and the round plays on
	// without it, losing only the artwork on the completion message.
	Song    db.Song
	HasSong bool
	Lines   []string
	// Cursor indexes Lines at the line currently being waited for. It starts at 1
	// because the bot posts Lines[0] itself.
	Cursor   int
	Complete bool
}

// Answer is the line this round is waiting to hear.
func (r SingAlongRound) Answer() string {
	if r.Cursor < 0 || r.Cursor >= len(r.Lines) {
		return ""
	}
	return r.Lines[r.Cursor]
}

// Judge decides what a message is.
//
// The previous line is scored alongside the current one because members read the
// screen: the line the bot posted, or the line somebody just won, is right there to
// be typed again. Whichever line the message is closer to is the one it is taken to
// be, so a chorus that literally repeats a line still counts as an answer rather
// than an echo.
func (r SingAlongRound) Judge(message string) SingAlongVerdict {
	if r.Complete {
		return SingAlongEcho
	}

	message = strings.TrimSpace(message)
	answer := r.Answer()
	if answer == "" || message == "" {
		return SingAlongWrong
	}

	current := lyricLineScore(answer, message)

	var previous float64
	if r.Cursor > 0 {
		previous = lyricLineScore(r.Lines[r.Cursor-1], message)
	}

	switch {
	case current >= SingAlongThreshold && current >= previous:
		return SingAlongCorrect
	case previous >= SingAlongThreshold:
		return SingAlongEcho
	default:
		return SingAlongWrong
	}
}

// SingAlongState holds the live round for every channel that has one.
//
// It exists so the MessageCreate listener costs a map read rather than a query. That
// listener fires for every message in every channel of every guild, and the
// overwhelming majority of them are not sing-along answers; making that case free is
// the whole point, which is also why it is keyed by CHANNEL -- a miss is then an
// immediate "not a sing-along channel" rather than a guild hit followed by a
// comparison.
//
// The database remains the authority -- see AdvanceSingAlongCursor -- and this is
// rebuilt from it on the scheduler's first pass after a restart.
type SingAlongState struct {
	mu     sync.RWMutex
	rounds map[snowflake.ID]SingAlongRound
}

func NewSingAlongState() *SingAlongState {
	return &SingAlongState{rounds: make(map[snowflake.ID]SingAlongRound)}
}

// Round returns the round running in a channel, if there is one.
func (s *SingAlongState) Round(channelID snowflake.ID) (SingAlongRound, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	round, ok := s.rounds[channelID]
	return round, ok
}

// Set installs a round, replacing whatever was there.
func (s *SingAlongState) Set(round SingAlongRound) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rounds[round.ChannelID] = round
}

// Advance moves the round on by one line, but only if it is still sitting where the
// caller thinks it is. It reports the new round and whether the caller won.
//
// This is a compare-and-swap rather than an increment because two members can answer
// the same line in the same instant and only one of them may be paid. Losing here
// costs nothing: the loser's message is simply left alone.
//
// The round is marked complete when the cursor runs off the end of the song.
func (s *SingAlongState) Advance(channelID snowflake.ID, from int) (SingAlongRound, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	round, ok := s.rounds[channelID]
	if !ok || round.Complete || round.Cursor != from {
		return round, false
	}

	round.Cursor++
	if round.Cursor >= len(round.Lines) {
		round.Complete = true
	}
	s.rounds[channelID] = round
	return round, true
}

// Complete latches a round finished without moving the cursor, for the caller that
// lost the race to the database.
func (s *SingAlongState) Complete(channelID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if round, ok := s.rounds[channelID]; ok {
		round.Complete = true
		s.rounds[channelID] = round
	}
}

// Retain drops every round whose channel is not in the given set.
//
// Called at the end of each scheduler cycle with the channels that are still
// configured. Without it a channel un-set in the dashboard would keep deleting
// members' messages until the bot was restarted.
func (s *SingAlongState) Retain(channelIDs []snowflake.ID) {
	keep := make(map[snowflake.ID]struct{}, len(channelIDs))
	for _, id := range channelIDs {
		keep[id] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.rounds {
		if _, ok := keep[id]; !ok {
			delete(s.rounds, id)
		}
	}
}

// Clear forgets one channel's round, for when the feature is switched off.
func (s *SingAlongState) Clear(channelID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rounds, channelID)
}
