-- name: GetSingAlongGuilds :many
-- Every guild with the feature switched on. Like GetAnniversaryGuilds this carries
-- the schedule and the cooldown alongside the channel, because the scheduler needs
-- all of them and none of the shared Get*NotificationChannels shapes fit.
SELECT guild_id,
       sing_along_channel,
       sing_along_hour,
       sing_along_timezone,
       sing_along_cooldown_minutes
FROM guilds
WHERE sing_along_channel IS NOT NULL;

-- name: GetSingAlongRound :one
SELECT * FROM sing_along_rounds WHERE guild_id = $1;

-- name: ClaimSingAlongDay :execrows
-- Claims a guild's round for one local day, atomically. Returns 1 when this caller
-- won the claim and must wipe the channel and post, 0 when the day was already
-- taken.
--
-- The upsert is the claim, so two ticks racing each other -- or two bot instances --
-- cannot both wipe a channel. The WHERE on the DO UPDATE is what makes it a claim
-- rather than a blind overwrite: a round already rolled for today, part-way through
-- its song, is left exactly as it is.
--
-- Claimed before the wipe, not after, for the same reason as ClaimAnniversaryDay: a
-- crash mid-send costs one day's lyric, while claiming afterwards would re-wipe the
-- channel on the next tick, and of the two, quiet is the safer failure.
--
-- IS DISTINCT FROM rather than "<", because a guild moved westward -- Auckland to
-- Los Angeles -- has its local date go BACKWARD by a day, and a "<" test would leave
-- the round unclaimable until the calendar caught up. The twelve hour floor on
-- started_at is what keeps that same looseness from re-wiping a channel an hour
-- after a timezone edit; the re-roll button is the escape hatch for anything this
-- refuses.
INSERT INTO sing_along_rounds (guild_id, local_date, song_id, lyric_lines)
VALUES ($1, $2, $3, $4)
ON CONFLICT (guild_id) DO UPDATE
   SET local_date   = EXCLUDED.local_date,
       song_id      = EXCLUDED.song_id,
       lyric_lines  = EXCLUDED.lyric_lines,
       cursor       = 1,
       started_at   = NOW(),
       completed_at = NULL
 WHERE sing_along_rounds.local_date IS DISTINCT FROM EXCLUDED.local_date
   AND sing_along_rounds.started_at < NOW() - INTERVAL '12 hours';

-- name: ReplaceSingAlongRound :exec
-- The same upsert without the date guard, for a re-roll asked for by hand. An admin
-- pressing the button has already said they want today's round replaced.
INSERT INTO sing_along_rounds (guild_id, local_date, song_id, lyric_lines)
VALUES ($1, $2, $3, $4)
ON CONFLICT (guild_id) DO UPDATE
   SET local_date   = EXCLUDED.local_date,
       song_id      = EXCLUDED.song_id,
       lyric_lines  = EXCLUDED.lyric_lines,
       cursor       = 1,
       started_at   = NOW(),
       completed_at = NULL;

-- name: AdvanceSingAlongCursor :execrows
-- Advances the round by one line. Returns 1 when this member's guess won the line
-- and 0 when someone else got there first.
--
-- The cursor is matched, not just incremented, so two correct guesses arriving in
-- the same instant cannot both be paid: the second finds the cursor already moved
-- and updates nothing. The in-memory round does the same compare-and-swap first;
-- this is what actually decides, because it is the only copy two bot instances
-- share.
UPDATE sing_along_rounds
   SET cursor = cursor + 1
 WHERE guild_id = $1 AND cursor = $2 AND completed_at IS NULL;

-- name: CompleteSingAlongRound :exec
UPDATE sing_along_rounds SET completed_at = NOW() WHERE guild_id = $1;

-- name: DeleteSingAlongRound :exec
DELETE FROM sing_along_rounds WHERE guild_id = $1;

-- name: SetSingAlongChannel :exec
UPDATE guilds SET sing_along_channel = $2 WHERE guild_id = $1;

-- name: SetSingAlongSchedule :exec
UPDATE guilds
SET sing_along_hour = $2, sing_along_timezone = $3, sing_along_cooldown_minutes = $4
WHERE guild_id = $1;
