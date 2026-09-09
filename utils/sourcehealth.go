package utils

import (
	"sort"
	"sync"
	"time"
)

// Names of the periodic content sources, used as keys into the registry.
const (
	SourceSTMPD    = "stmpd"
	SourceBeatport = "beatport"
	SourceReddit   = "reddit"
	SourceYouTube  = "youtube"
	SourceTour     = "tour"
	// SourceAnniversary tracks liveness, not fetching. The catalogue is local, so
	// there is nothing to fail at; a cycle counts as a success whenever the guild
	// configs could be read. Recording success only when a post went out would read
	// as "last success 3 days ago" on any quiet stretch -- which is precisely the
	// silent-failure signature this registry was written to catch, and it would cry
	// wolf on days that simply have no anniversary.
	SourceAnniversary = "anniversary"
	// SourceLyrics follows the same rule as SourceAnniversary and for the same
	// reason: the backlog is meant to drain. Once it has, there is nothing to fetch,
	// and treating an empty queue as a failed cycle would report the source degraded
	// exactly when it has finished its job. A cycle is a success whenever the queue
	// could be read and nothing failed at the HTTP layer.
	SourceLyrics = "lrclib"
	// SourceSingAlong follows the same rule as SourceAnniversary, and for the same
	// reason: there is nothing remote to fetch, and a guild that has already had its
	// lyric for the day is the normal state rather than a failure. A cycle counts as
	// a success whenever the guild configs could be read.
	SourceSingAlong = "sing-along"
)

// sourceFailureThreshold is how many consecutive bad cycles a source may have
// before it is reported as degraded. One is too eager -- a single timeout is
// normal -- and the fetchers run every few minutes, so two still surfaces a real
// outage well inside the hour.
const sourceFailureThreshold = 2

// SourceState is a point-in-time view of one content source.
type SourceState struct {
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastSuccess         time.Time `json:"last_success,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	EverSucceeded       bool      `json:"ever_succeeded"`
}

// Degraded reports whether the source has failed often enough to be worth
// surfacing. A source that has never succeeded is not degraded: it may simply be
// unconfigured, and reporting it would make /health noisy for every deployment
// that does not use every feed.
func (s SourceState) Degraded() bool {
	return s.EverSucceeded && s.ConsecutiveFailures >= sourceFailureThreshold
}

// sourceHealth tracks whether each periodic fetcher is actually working.
//
// This exists because the failure that motivated it was invisible: Beatport
// authentication broke on 2026-08-25 and the fetcher went on logging "Running
// Beatport releases fetcher" and "sync complete new=0 updated=0 skipped=0" every
// 15 minutes for four days. Every individual line looked fine. Nothing aggregated
// them into "this source has produced nothing since Tuesday".
var sourceHealth = struct {
	mu     sync.RWMutex
	states map[string]*SourceState
}{states: make(map[string]*SourceState)}

// RecordSourceSuccess marks a cycle that produced usable results.
func RecordSourceSuccess(name string) {
	sourceHealth.mu.Lock()
	defer sourceHealth.mu.Unlock()

	st := sourceHealth.states[name]
	if st == nil {
		st = &SourceState{}
		sourceHealth.states[name] = st
	}
	st.ConsecutiveFailures = 0
	st.LastSuccess = time.Now()
	st.LastError = ""
	st.EverSucceeded = true
}

// RecordSourceFailure marks a cycle that failed or returned nothing.
//
// Callers should treat an empty result as a failure when the source has produced
// results before. "Zero releases today" and "the scrape silently stopped working"
// are indistinguishable from a single cycle, and only one of them is normal.
func RecordSourceFailure(name string, err error) {
	sourceHealth.mu.Lock()
	defer sourceHealth.mu.Unlock()

	st := sourceHealth.states[name]
	if st == nil {
		st = &SourceState{}
		sourceHealth.states[name] = st
	}
	st.ConsecutiveFailures++
	if err != nil {
		st.LastError = err.Error()
	}
}

// SourceHealthSnapshot returns a copy of the current state of every source.
func SourceHealthSnapshot() map[string]SourceState {
	sourceHealth.mu.RLock()
	defer sourceHealth.mu.RUnlock()

	out := make(map[string]SourceState, len(sourceHealth.states))
	for name, st := range sourceHealth.states {
		out[name] = *st
	}
	return out
}

// DegradedSources returns the names of sources currently failing, sorted so the
// health response is stable between calls.
func DegradedSources() []string {
	var names []string
	for name, st := range SourceHealthSnapshot() {
		if st.Degraded() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
