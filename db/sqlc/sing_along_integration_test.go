//go:build integration

package db_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
)

var singAlongSuffix atomic.Int64

func singAlongGuild(t *testing.T) int64 {
	t.Helper()

	guildID := 800_000_000_000_000_000 + singAlongSuffix.Add(1)
	ctx := context.Background()

	if _, err := testPool.Exec(ctx, "INSERT INTO guilds (guild_id) VALUES ($1)", guildID); err != nil {
		t.Fatalf("failed to create guild: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(ctx, "DELETE FROM guilds WHERE guild_id = $1", guildID); err != nil {
			t.Errorf("failed to clean up guild %d: %v", guildID, err)
		}
	})
	return guildID
}

func localDate(day int) pgtype.Date {
	return pgtype.Date{
		Time:  time.Date(2026, time.March, day, 0, 0, 0, 0, time.UTC),
		Valid: true,
	}
}

func claim(t *testing.T, q *db.Queries, guildID int64, date pgtype.Date, lines []string) int64 {
	t.Helper()

	rows, err := q.ClaimSingAlongDay(context.Background(), db.ClaimSingAlongDayParams{
		GuildID:    guildID,
		LocalDate:  date,
		LyricLines: lines,
	})
	if err != nil {
		t.Fatalf("ClaimSingAlongDay failed: %v", err)
	}
	return rows
}

// backdate ages the round, since the claim will not replace one started in the
// last twelve hours.
func backdate(t *testing.T, guildID int64) {
	t.Helper()

	if _, err := testPool.Exec(context.Background(),
		"UPDATE sing_along_rounds SET started_at = NOW() - INTERVAL '2 days' WHERE guild_id = $1",
		guildID); err != nil {
		t.Fatalf("failed to backdate the round: %v", err)
	}
}

func TestClaimSingAlongDay_TakesTheDayOnce(t *testing.T) {
	q := queries(t)
	guildID := singAlongGuild(t)
	lines := []string{"one line here", "two lines here"}

	if rows := claim(t, q, guildID, localDate(14), lines); rows != 1 {
		t.Fatalf("the first claim of a day returned %d rows, want 1", rows)
	}

	// The same day again is the case that matters: without this the five minute
	// ticker would wipe the channel every cycle until midnight.
	if rows := claim(t, q, guildID, localDate(14), lines); rows != 0 {
		t.Errorf("re-claiming the same day returned %d rows, want 0", rows)
	}
}

func TestClaimSingAlongDay_TheNextDayReplacesTheRound(t *testing.T) {
	q := queries(t)
	guildID := singAlongGuild(t)

	claim(t, q, guildID, localDate(14), []string{"one line here", "two lines here"})

	// Advance into the song first, so the reset is visible.
	if _, err := q.AdvanceSingAlongCursor(context.Background(), db.AdvanceSingAlongCursorParams{
		GuildID: guildID, Cursor: 1,
	}); err != nil {
		t.Fatalf("AdvanceSingAlongCursor failed: %v", err)
	}
	backdate(t, guildID)

	if rows := claim(t, q, guildID, localDate(15), []string{"a new song line", "and its second"}); rows != 1 {
		t.Fatalf("the next day returned %d rows, want 1", rows)
	}

	round, err := q.GetSingAlongRound(context.Background(), guildID)
	if err != nil {
		t.Fatalf("GetSingAlongRound failed: %v", err)
	}
	if round.Cursor != 1 {
		t.Errorf("cursor = %d, want the new round to start at 1", round.Cursor)
	}
	if round.CompletedAt.Valid {
		t.Error("a new round should not be completed")
	}
	if len(round.LyricLines) != 2 || round.LyricLines[0] != "a new song line" {
		t.Errorf("lyric_lines = %q, want the new song's lines", round.LyricLines)
	}
}

// A guild moved westward -- Auckland to Los Angeles -- has its local date go
// BACKWARD by a day. A "<" predicate would leave the round unclaimable until the
// calendar caught up, silently, for a whole day.
func TestClaimSingAlongDay_SurvivesMovingTheClockBackward(t *testing.T) {
	q := queries(t)
	guildID := singAlongGuild(t)

	claim(t, q, guildID, localDate(15), []string{"one line here", "two lines here"})
	backdate(t, guildID)

	if rows := claim(t, q, guildID, localDate(14), []string{"a new song line", "and its second"}); rows != 1 {
		t.Errorf("an earlier local date returned %d rows, want 1", rows)
	}
}

// The other half of that looseness: a timezone edit an hour after the day's lyric
// went out must not wipe a channel people are already singing in.
func TestClaimSingAlongDay_WillNotRewipeAFreshRound(t *testing.T) {
	q := queries(t)
	guildID := singAlongGuild(t)

	claim(t, q, guildID, localDate(15), []string{"one line here", "two lines here"})

	if rows := claim(t, q, guildID, localDate(14), []string{"a new song line", "and its second"}); rows != 0 {
		t.Errorf("a round started minutes ago was replaced (%d rows), want 0", rows)
	}
}

func TestAdvanceSingAlongCursor_HasExactlyOneWinner(t *testing.T) {
	q := queries(t)
	guildID := singAlongGuild(t)

	claim(t, q, guildID, localDate(14), []string{"one line here", "two lines here", "three lines here"})

	// Sixteen members answering the same line at once. Only one may be paid.
	var (
		wg    sync.WaitGroup
		wins  atomic.Int64
		start = make(chan struct{})
	)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rows, err := db.New(testPool).AdvanceSingAlongCursor(context.Background(),
				db.AdvanceSingAlongCursorParams{GuildID: guildID, Cursor: 1})
			if err != nil {
				t.Errorf("AdvanceSingAlongCursor failed: %v", err)
				return
			}
			wins.Add(rows)
		}()
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Errorf("got %d winners for one line, want 1", got)
	}

	round, err := q.GetSingAlongRound(context.Background(), guildID)
	if err != nil {
		t.Fatalf("GetSingAlongRound failed: %v", err)
	}
	if round.Cursor != 2 {
		t.Errorf("cursor = %d, want 2", round.Cursor)
	}
}

func TestAdvanceSingAlongCursor_StopsOnACompletedRound(t *testing.T) {
	q := queries(t)
	ctx := context.Background()
	guildID := singAlongGuild(t)

	claim(t, q, guildID, localDate(14), []string{"one line here", "two lines here"})

	if err := q.CompleteSingAlongRound(ctx, guildID); err != nil {
		t.Fatalf("CompleteSingAlongRound failed: %v", err)
	}

	rows, err := q.AdvanceSingAlongCursor(ctx, db.AdvanceSingAlongCursorParams{
		GuildID: guildID, Cursor: 1,
	})
	if err != nil {
		t.Fatalf("AdvanceSingAlongCursor failed: %v", err)
	}
	if rows != 0 {
		t.Errorf("a completed round advanced (%d rows), want 0", rows)
	}
}

// The dashboard's merge tool DELETES song rows. A merge landing mid-round must not
// take the round with it -- lyric_lines is frozen, so the song is only needed for
// the completion message's artwork.
func TestSingAlongRound_SurvivesTheSongBeingMergedAway(t *testing.T) {
	q := queries(t)
	ctx := context.Background()
	guildID := singAlongGuild(t)

	var songID int64
	if err := testPool.QueryRow(ctx,
		`INSERT INTO songs (name, artists) VALUES ('Sing Along Fixture', 'Martin Garrix') RETURNING id`).
		Scan(&songID); err != nil {
		t.Fatalf("failed to create the fixture song: %v", err)
	}

	if _, err := q.ClaimSingAlongDay(ctx, db.ClaimSingAlongDayParams{
		GuildID:    guildID,
		LocalDate:  localDate(14),
		SongID:     pgtype.Int8{Int64: songID, Valid: true},
		LyricLines: []string{"one line here", "two lines here"},
	}); err != nil {
		t.Fatalf("ClaimSingAlongDay failed: %v", err)
	}

	if _, err := testPool.Exec(ctx, "DELETE FROM songs WHERE id = $1", songID); err != nil {
		t.Fatalf("failed to delete the fixture song: %v", err)
	}

	round, err := q.GetSingAlongRound(ctx, guildID)
	if err != nil {
		t.Fatalf("the round did not survive the merge: %v", err)
	}
	if round.SongID.Valid {
		t.Error("song_id should have been cleared, not left dangling")
	}
	if len(round.LyricLines) != 2 {
		t.Errorf("lyric_lines = %q, want the frozen lines to survive", round.LyricLines)
	}
}

func TestAwardCoins_ReportsWhenThereIsNobodyToPay(t *testing.T) {
	q := queries(t)
	ctx := context.Background()
	guildID := singAlongGuild(t)

	// users has no foreign key to guilds, so the row this test creates outlives the
	// guild cleanup -- and the guild IDs are deterministic per run. Without an
	// explicit cleanup the next run finds the member already there and the first
	// assertion, which is the whole point of the test, quietly stops testing it.
	userID := int64(1234)
	t.Cleanup(func() {
		if _, err := testPool.Exec(ctx,
			"DELETE FROM users WHERE id = $1 AND guild_id = $2", userID, guildID); err != nil {
			t.Errorf("failed to clean up user: %v", err)
		}
	})

	// A member with no users row. AddCoins would report success and pay nothing,
	// which for the sing-along means the reward for playing vanishes in silence.
	rows, err := q.AwardCoins(ctx, db.AwardCoinsParams{ID: userID, GuildID: guildID, InHand: 25})
	if err != nil {
		t.Fatalf("AwardCoins failed: %v", err)
	}
	if rows != 0 {
		t.Fatalf("AwardCoins paid a member who does not exist (%d rows)", rows)
	}

	if _, err := q.CreateUser(ctx, db.CreateUserParams{ID: userID, GuildID: guildID}); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	rows, err = q.AwardCoins(ctx, db.AwardCoinsParams{ID: userID, GuildID: guildID, InHand: 25})
	if err != nil {
		t.Fatalf("AwardCoins failed: %v", err)
	}
	if rows != 1 {
		t.Errorf("AwardCoins returned %d rows after the member existed, want 1", rows)
	}
}
