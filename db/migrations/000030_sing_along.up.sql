-- Per-guild sing-along configuration. The channel follows the convention every
-- other channel column uses: NULL means the feature is off, so no separate toggle
-- is needed.
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS sing_along_channel BIGINT;

-- Local hour of day to drop the day's lyric at, and the zone that hour is read in.
-- Same reasoning as the anniversary columns in 000017: this is not "post when a
-- source changes" but "post in this server's morning", so the schedule lives per
-- guild. The CHECK rides on the ADD COLUMN so the whole statement is skipped when
-- the column already exists.
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS sing_along_hour INTEGER NOT NULL DEFAULT 9
    CHECK (sing_along_hour BETWEEN 0 AND 23);

-- IANA name, validated with utils.ValidateTimezone before it is ever written here.
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS sing_along_timezone TEXT NOT NULL DEFAULT 'UTC';

-- How long a member must wait between attempts. This is not enforced in Go: it is
-- pushed to the channel's own slowmode (rate_limit_per_user), so Discord does the
-- waiting and a member who guesses wrong burns their own window. Discord's ceiling
-- is 21600 seconds, which is why 360 minutes is the maximum; 0 turns slowmode off.
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS sing_along_cooldown_minutes INTEGER NOT NULL DEFAULT 10
    CHECK (sing_along_cooldown_minutes BETWEEN 0 AND 360);

-- The live round. One row per guild, not one per day as anniversary_posts has:
-- a round is state that is read and advanced all day rather than a receipt written
-- once. local_date on the same row still serves as the replay lock, keyed on the
-- guild's LOCAL date because that is what "today" means for that guild -- for a
-- server at UTC+13 the local and UTC days disagree for half of every day.
--
-- lyric_lines is frozen at roll time rather than re-split from songs.lyrics on
-- every guess. The lyrics backfill and the dashboard's song editor can both rewrite
-- that column mid-round, and re-deriving would shift the cursor under members who
-- are halfway through the song.
--
-- cursor is the index into lyric_lines of the line currently being waited for. It
-- starts at 1 because the bot itself posts line 0.
--
-- song_id is nullable and clears rather than cascades. The dashboard's merge tool
-- DELETES the losing song row, and a merge landing mid-round would otherwise take
-- the whole round with it: the channel would be left holding a half-sung song with
-- no state behind it and nothing to recover until the next day. Because lyric_lines
-- is frozen the round plays on perfectly well without the song; all that is lost is
-- the artwork and the links on the completion message.
CREATE TABLE IF NOT EXISTS sing_along_rounds (
    guild_id     BIGINT      PRIMARY KEY REFERENCES guilds(guild_id) ON DELETE CASCADE,
    local_date   DATE        NOT NULL,
    song_id      BIGINT      REFERENCES songs(id) ON DELETE SET NULL,
    lyric_lines  TEXT[]      NOT NULL,
    cursor       INTEGER     NOT NULL DEFAULT 1,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ
);
