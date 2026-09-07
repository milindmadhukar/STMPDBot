-- name: GetSong :one
-- IS NOT DISTINCT FROM, not "=": release_date is NULL for unreleased songs and for
-- ones whose date we could not establish, and "=" never matches NULL.
SELECT * FROM songs
WHERE name = $1 AND artists = $2 AND release_date IS NOT DISTINCT FROM sqlc.narg(release_date);

-- name: GetSongByID :one
SELECT * FROM songs WHERE id = $1;

-- name: GetSongByBeatportID :one
SELECT * FROM songs WHERE beatport_id = $1;

-- name: SearchSongsForAgent :many
-- Backs the AI persona feature's "search_songs" tool (stmpdbot/ai). Same term
-- matching as GetSongsLike below, but also returns is_unreleased and genre so
-- the model can answer "favourite unreleased AREA21 track"-shaped questions
-- from the rows themselves instead of typing "unreleased" into the search
-- terms (which would never match search_text and return nothing) or, worse,
-- inventing a track name that was never in the catalogue.
SELECT id, name, artists, mix_name, release_date, is_unreleased, genre
FROM songs
WHERE NOT is_collection
  AND COALESCE(search_text,
               LOWER(artists || ' ' || name || ' ' || COALESCE(mix_name, '')))
        LIKE ALL ($1::text[])
ORDER BY (parent_song_id IS NOT NULL), release_date DESC
LIMIT 20;

-- name: GetSongsLike :many
-- Renditions are listed, not hidden. A remix is its own recording with its own links,
-- and excluding it made those links unreachable through the bot at all. What made the
-- old list unusable was the same song appearing twice under one name -- that is a
-- duplicate, and duplicates are merged rather than filtered out of sight.
--
-- Collections are still excluded: an EP is not something to fetch links for as a song.
-- mix_name comes back so the caller can tell two renditions apart in the label.
--
-- Matching is per term against the folded haystack, not one contiguous LIKE over the
-- raw columns. The old form could not find "Don't Tell Me" by "Matisse & Sadko, Aspyer,
-- Matluck" from "matisse sadko dont tell me": every word is in the row, but not
-- adjacent and not in that order, so the song read as missing from the catalogue.
--
-- COALESCE onto an unfolded expression so a row inserted before the next rekey-songs is
-- still findable, just without accent and apostrophe folding.
SELECT id, name, artists, mix_name, release_date
FROM songs
WHERE NOT is_collection
  AND COALESCE(search_text,
               LOWER(artists || ' ' || name || ' ' || COALESCE(mix_name, '')))
        LIKE ALL (sqlc.arg(terms)::text[])
ORDER BY (parent_song_id IS NOT NULL), release_date DESC
LIMIT 20;

-- name: GetRandomSongNames :many
SELECT id, name, artists, mix_name, release_date
FROM songs
WHERE NOT is_collection AND parent_song_id IS NULL
ORDER BY RANDOM()
LIMIT 20;

-- name: GetSongsWithLyricsLike :many
-- Canonical rows only, which is the opposite of what GetSongsLike does and is
-- deliberate. A remix carries its own streaming links, so /links has to list it; a
-- remix's words are byte-identical to the original's, so listing it in /lyrics is
-- eighteen ways to read the same page.
--
-- This became visible when the LRCLIB backfill fanned lyrics out to renditions:
-- searching "scared to be lonely" returned the song plus seventeen of its remixes and
-- pushed everything else past Discord's 20-choice limit. Before the fan-out only the
-- canonical row had lyrics, so the missing filter never showed.
--
-- GetRandomSongNamesWithLyrics, which answers the same autocomplete on empty input,
-- has always filtered this way. The two now agree.
--
-- Term matching is the same as GetSongsLike; see the note there.
SELECT id, name, artists, mix_name, release_date
FROM songs
WHERE lyrics IS NOT NULL
  AND NOT is_collection
  AND parent_song_id IS NULL
  AND COALESCE(search_text,
               LOWER(artists || ' ' || name || ' ' || COALESCE(mix_name, '')))
        LIKE ALL (sqlc.arg(terms)::text[])
ORDER BY release_date DESC
LIMIT 20;

-- name: GetRandomSongNamesWithLyrics :many
SELECT id, name, artists, mix_name, release_date
FROM songs
WHERE lyrics IS NOT NULL AND NOT is_collection AND parent_song_id IS NULL
ORDER BY RANDOM()
LIMIT 20;

-- name: GetRandomSongWithLyrics :one
-- '%ytram%' was '%ytrram%'. The typo meant no Ytram song has ever been served by
-- the quiz; config.toml's monitored-artist list confirms the correct spelling.
--
-- Crediting one of the monitored acts is not the same as the song being theirs. On a
-- remix the credit line names both acts, so "The Weeknd, Martin Garrix" matched here
-- and the quiz asked people to name a Weeknd record from Weeknd lyrics -- the answer
-- "Can't Feel My Face" is not a Martin Garrix song, it is a Martin Garrix remix of
-- someone else's. Same for "Cocoon", which is 070 Shake's.
--
-- mix_name is what separates the two cases, because it names the remixer and not the
-- artist. Our act in the remixer slot means the record belongs to whoever we remixed,
-- so it is dropped; anyone else there ("La La La (Drove Remix)") means one of our own
-- songs in someone else's hands, whose lyrics are still ours to quiz on.
SELECT * FROM songs
WHERE lyrics IS NOT NULL
AND NOT is_instrumental
AND NOT is_collection
AND parent_song_id IS NULL
AND (LOWER(artists) LIKE '%martin garrix%'
   OR LOWER(artists) LIKE '%area21%'
   OR LOWER(artists) LIKE '%ytram%'
   OR LOWER(artists) LIKE '%grx%')
AND NOT (LOWER(COALESCE(mix_name, '')) LIKE '%remix%'
   AND (LOWER(mix_name) LIKE '%martin garrix%'
      OR LOWER(mix_name) LIKE '%area21%'
      OR LOWER(mix_name) LIKE '%ytram%'
      OR LOWER(mix_name) LIKE '%grx%'))
ORDER BY RANDOM()
LIMIT 1;

-- name: GetRandomSongWithLyricsEasy :one
-- Carries the same remix exclusion as GetRandomSongWithLyrics above, for the same
-- reason: "easy" narrows which acts qualify, not what counts as their song.
SELECT * FROM songs
WHERE lyrics IS NOT NULL
AND NOT is_instrumental
AND NOT is_collection
AND parent_song_id IS NULL
AND LOWER(artists) LIKE '%martin garrix%'
AND NOT (LOWER(COALESCE(mix_name, '')) LIKE '%remix%'
   AND (LOWER(mix_name) LIKE '%martin garrix%'
      OR LOWER(mix_name) LIKE '%area21%'
      OR LOWER(mix_name) LIKE '%ytram%'
      OR LOWER(mix_name) LIKE '%grx%'))
ORDER BY RANDOM()
LIMIT 1;

-- name: InsertRelease :one
INSERT INTO songs (
    name, artists, mix_name, release_date, thumbnail_url, stmpd_slug,
    spotify_url, apple_music_url, youtube_url, youtube_music_url,
    deezer_url, tidal_url, amazon_music_url, beatport_url, beatport_release_id,
    stmpd_synced_at, source
) VALUES (
    sqlc.arg(name), sqlc.arg(artists), sqlc.narg(mix_name), sqlc.arg(release_date), sqlc.narg(thumbnail_url), sqlc.narg(stmpd_slug),
    sqlc.narg(spotify_url), sqlc.narg(apple_music_url), sqlc.narg(youtube_url), sqlc.narg(youtube_music_url),
    sqlc.narg(deezer_url), sqlc.narg(tidal_url), sqlc.narg(amazon_music_url),
    sqlc.narg(beatport_url), sqlc.narg(beatport_release_id),
    NOW(), 'stmpd'
)
RETURNING *;

-- name: GetSongByStmpdSlug :one
SELECT * FROM songs WHERE stmpd_slug = $1;

-- name: UpdateSongWithStmpdRelease :execrows
-- The full-fidelity counterpart to UpdateSongWithStmpdLinks: everything the STMPD
-- dataset knows about a release, applied to a row that already exists.
--
-- release_date is COALESCEd rather than left alone because the dataset carries the
-- exact date and many stored rows carry a "<year>-01-01" placeholder. Correcting it
-- is safe now only because announcement is gated on songs.announced_at, which every
-- pre-existing row already has stamped -- without that watermark this UPDATE would
-- make the back catalogue look new.
--
-- Every column a person can correct is wrapped in a locked_fields test, and the same
-- test is mirrored in the WHERE. Both halves are needed: guarding only the SET list
-- leaves the WHERE matching forever, so :execrows reports a write every cycle and the
-- churn the IS DISTINCT FROM guards exist to kill comes straight back.
--
-- The re-arm pair below additionally requires release_date to be unlocked. Without that
-- a row whose date someone set by hand would have its announcement re-armed on the next
-- sync and be posted to every server a second time.
--
-- thumbnail_url is only ever filled in, never overwritten: roughly 800 of the 1015
-- releases have no artwork in the dataset, and an empty value there means "unknown",
-- not "this release has none".
UPDATE songs SET
    stmpd_slug          = COALESCE(sqlc.narg(stmpd_slug),          stmpd_slug),
    release_date        = CASE WHEN 'release_date' = ANY(locked_fields) THEN release_date
                               ELSE COALESCE(sqlc.narg(release_date), release_date) END,
    -- The catalogue is the authority for what a release is called and which
    -- rendition it is, so both are taken from it rather than merged with whatever
    -- the row happened to hold. This is what keeps the shape uniform: the name is
    -- the song's name and the rendition lives in mix_name, never smuggled into the
    -- title for some rows and not others.
    name                = CASE WHEN 'name' = ANY(locked_fields) THEN name
                               ELSE COALESCE(sqlc.narg(title), name) END,
    mix_name            = CASE WHEN 'mix_name' = ANY(locked_fields) THEN mix_name
                               ELSE sqlc.narg(mix_name) END,
    -- normalized_name is derived from name in Go, so a name the catalogue rewrote
    -- here invalidates it. Cleared rather than recomputed: SQL cannot derive it, and
    -- NULL falls back to computing on read, whereas a leftover value would be
    -- confidently wrong. The handler re-reads the row and refills this immediately.
    normalized_name     = CASE WHEN name IS DISTINCT FROM COALESCE(sqlc.narg(title), name)
                               THEN NULL ELSE normalized_name END,
    spotify_url = CASE WHEN 'spotify_url' = ANY(locked_fields) THEN spotify_url
                               ELSE COALESCE(sqlc.narg(spotify_url), spotify_url) END,
    apple_music_url = CASE WHEN 'apple_music_url' = ANY(locked_fields) THEN apple_music_url
                               ELSE COALESCE(sqlc.narg(apple_music_url), apple_music_url) END,
    youtube_url = CASE WHEN 'youtube_url' = ANY(locked_fields) THEN youtube_url
                               ELSE COALESCE(sqlc.narg(youtube_url), youtube_url) END,
    youtube_music_url = CASE WHEN 'youtube_music_url' = ANY(locked_fields) THEN youtube_music_url
                               ELSE COALESCE(sqlc.narg(youtube_music_url), youtube_music_url) END,
    deezer_url = CASE WHEN 'deezer_url' = ANY(locked_fields) THEN deezer_url
                               ELSE COALESCE(sqlc.narg(deezer_url), deezer_url) END,
    tidal_url = CASE WHEN 'tidal_url' = ANY(locked_fields) THEN tidal_url
                               ELSE COALESCE(sqlc.narg(tidal_url), tidal_url) END,
    amazon_music_url = CASE WHEN 'amazon_music_url' = ANY(locked_fields) THEN amazon_music_url
                               ELSE COALESCE(sqlc.narg(amazon_music_url), amazon_music_url) END,
    beatport_url = CASE WHEN 'beatport_url' = ANY(locked_fields) THEN beatport_url
                               ELSE COALESCE(sqlc.narg(beatport_url), beatport_url) END,
    beatport_release_id = COALESCE(sqlc.narg(beatport_release_id), beatport_release_id),
    thumbnail_url       = CASE WHEN COALESCE(thumbnail_url, '') = ''
                                     AND NOT ('thumbnail_url' = ANY(locked_fields))
                               THEN sqlc.narg(thumbnail_url) ELSE thumbnail_url END,
    -- A song someone added because they heard it played, which has now actually
    -- come out. Clearing announced_at re-arms the announcement: the row is old, but
    -- the release is news, and this is the one case where re-announcing is right.
    --
    -- The ::text casts are load-bearing. IS NOT NULL accepts any type, so these are
    -- the only places the parameter appears without a type to infer from, and
    -- Postgres rejects the whole statement with 42P08 at execution time.
    is_unreleased = CASE WHEN sqlc.narg(release_date)::text IS NOT NULL
                              AND NOT ('release_date' = ANY(locked_fields))
                              AND NOT ('is_unreleased' = ANY(locked_fields))
                         THEN FALSE ELSE is_unreleased END,
    announced_at  = CASE WHEN is_unreleased AND sqlc.narg(release_date)::text IS NOT NULL
                              AND NOT ('release_date' = ANY(locked_fields))
                         THEN NULL ELSE announced_at END,
    stmpd_synced_at     = NOW()
WHERE id = sqlc.arg(id)
  AND (
       (is_unreleased AND sqlc.narg(release_date)::text IS NOT NULL
        AND NOT ('release_date' = ANY(locked_fields)))
    OR stmpd_slug          IS DISTINCT FROM COALESCE(sqlc.narg(stmpd_slug),          stmpd_slug)
    OR (NOT ('release_date' = ANY(locked_fields))
        AND release_date IS DISTINCT FROM COALESCE(sqlc.narg(release_date), release_date))
    OR (NOT ('name' = ANY(locked_fields))
        AND name IS DISTINCT FROM COALESCE(sqlc.narg(title), name))
    OR (NOT ('mix_name' = ANY(locked_fields))
        AND mix_name IS DISTINCT FROM sqlc.narg(mix_name))
    OR (NOT ('spotify_url' = ANY(locked_fields))
        AND spotify_url IS DISTINCT FROM COALESCE(sqlc.narg(spotify_url), spotify_url))
    OR (NOT ('apple_music_url' = ANY(locked_fields))
        AND apple_music_url IS DISTINCT FROM COALESCE(sqlc.narg(apple_music_url), apple_music_url))
    OR (NOT ('youtube_url' = ANY(locked_fields))
        AND youtube_url IS DISTINCT FROM COALESCE(sqlc.narg(youtube_url), youtube_url))
    OR (NOT ('youtube_music_url' = ANY(locked_fields))
        AND youtube_music_url IS DISTINCT FROM COALESCE(sqlc.narg(youtube_music_url), youtube_music_url))
    OR (NOT ('deezer_url' = ANY(locked_fields))
        AND deezer_url IS DISTINCT FROM COALESCE(sqlc.narg(deezer_url), deezer_url))
    OR (NOT ('tidal_url' = ANY(locked_fields))
        AND tidal_url IS DISTINCT FROM COALESCE(sqlc.narg(tidal_url), tidal_url))
    OR (NOT ('amazon_music_url' = ANY(locked_fields))
        AND amazon_music_url IS DISTINCT FROM COALESCE(sqlc.narg(amazon_music_url), amazon_music_url))
    OR (NOT ('beatport_url' = ANY(locked_fields))
        AND beatport_url IS DISTINCT FROM COALESCE(sqlc.narg(beatport_url), beatport_url))
    OR beatport_release_id IS DISTINCT FROM COALESCE(sqlc.narg(beatport_release_id), beatport_release_id)
    -- Cast for the same reason as in UpdateSongWithStmpdLinks: IS NOT NULL leaves
    -- the parameter's type undetermined and Postgres raises 42P08.
    OR (COALESCE(thumbnail_url, '') = '' AND sqlc.narg(thumbnail_url)::text IS NOT NULL
        AND NOT ('thumbnail_url' = ANY(locked_fields)))
    OR stmpd_synced_at IS NULL
  );

-- name: InsertBeatportSong :one
INSERT INTO songs (
    name, artists, release_date, thumbnail_url, beatport_id, mix_name,
    release_name, genre, sub_genre, bpm, musical_key, length_ms, beatport_slug, source
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'beatport')
RETURNING *;

-- name: DoesSongExist :one
SELECT EXISTS(SELECT 1 FROM songs
  WHERE name = $1 AND artists = $2 AND release_date IS NOT DISTINCT FROM sqlc.narg(release_date));

-- name: DoesBeatportSongExist :one
SELECT EXISTS(SELECT 1 FROM songs WHERE beatport_id = $1);

-- name: UpdateSongWithBeatportData :execrows
-- :execrows plus the IS DISTINCT FROM guard below make this a no-op when the row
-- already holds this data. Without it every cycle rewrote the same ~73 rows and
-- reported updated=73 forever, which hid whether anything had actually changed.
-- thumbnail_url is COALESCEd rather than assigned: a beatport track with no square
-- artwork arrives as NULL and must not erase artwork another source already found.
--
-- Every editable column is wrapped in a locked_fields test, and the same test is
-- repeated in the WHERE. Both halves are needed. Guarding only the SET list would leave
-- the WHERE matching forever: the other assignments still fire, :execrows keeps
-- returning 1 every cycle, and the write churn the IS DISTINCT FROM guards were added
-- to kill comes straight back.
--
-- beatport_updated is deliberately unguarded -- it is the fetcher's own "already
-- enriched" sentinel, and locking it would stall the fetcher rather than protect
-- anything a person typed.
UPDATE songs SET
    name          = CASE WHEN 'name' = ANY(locked_fields) THEN name ELSE sqlc.arg(name) END,
    artists       = CASE WHEN 'artists' = ANY(locked_fields) THEN artists ELSE sqlc.arg(artists) END,
    thumbnail_url = CASE WHEN 'thumbnail_url' = ANY(locked_fields) THEN thumbnail_url
                         ELSE COALESCE(sqlc.narg(thumbnail_url), thumbnail_url) END,
    beatport_id   = sqlc.narg(beatport_id),
    beatport_slug = COALESCE(sqlc.narg(beatport_slug), beatport_slug),
    mix_name      = CASE WHEN 'mix_name' = ANY(locked_fields) THEN mix_name ELSE sqlc.narg(mix_name) END,
    release_date  = CASE WHEN 'release_date' = ANY(locked_fields) THEN release_date ELSE sqlc.arg(release_date) END,
    release_name  = CASE WHEN 'release_name' = ANY(locked_fields) THEN release_name ELSE sqlc.narg(release_name) END,
    genre         = CASE WHEN 'genre' = ANY(locked_fields) THEN genre ELSE sqlc.narg(genre) END,
    sub_genre     = CASE WHEN 'sub_genre' = ANY(locked_fields) THEN sub_genre ELSE sqlc.narg(sub_genre) END,
    bpm           = CASE WHEN 'bpm' = ANY(locked_fields) THEN bpm ELSE sqlc.narg(bpm) END,
    musical_key   = CASE WHEN 'musical_key' = ANY(locked_fields) THEN musical_key ELSE sqlc.narg(musical_key) END,
    length_ms     = sqlc.narg(length_ms),
    beatport_updated = TRUE
WHERE id = sqlc.arg(id)
  AND (
       (NOT ('name' = ANY(locked_fields))          AND name          IS DISTINCT FROM sqlc.arg(name))
    OR (NOT ('artists' = ANY(locked_fields))       AND artists       IS DISTINCT FROM sqlc.arg(artists))
    OR (NOT ('thumbnail_url' = ANY(locked_fields)) AND thumbnail_url IS DISTINCT FROM COALESCE(sqlc.narg(thumbnail_url), thumbnail_url))
    OR beatport_id   IS DISTINCT FROM sqlc.narg(beatport_id)
    OR beatport_slug IS DISTINCT FROM COALESCE(sqlc.narg(beatport_slug), beatport_slug)
    OR (NOT ('mix_name' = ANY(locked_fields))      AND mix_name      IS DISTINCT FROM sqlc.narg(mix_name))
    OR (NOT ('release_date' = ANY(locked_fields))  AND release_date  IS DISTINCT FROM sqlc.arg(release_date))
    OR (NOT ('release_name' = ANY(locked_fields))  AND release_name  IS DISTINCT FROM sqlc.narg(release_name))
    OR (NOT ('genre' = ANY(locked_fields))         AND genre         IS DISTINCT FROM sqlc.narg(genre))
    OR (NOT ('sub_genre' = ANY(locked_fields))     AND sub_genre     IS DISTINCT FROM sqlc.narg(sub_genre))
    OR (NOT ('bpm' = ANY(locked_fields))           AND bpm           IS DISTINCT FROM sqlc.narg(bpm))
    OR (NOT ('musical_key' = ANY(locked_fields))   AND musical_key   IS DISTINCT FROM sqlc.narg(musical_key))
    OR length_ms     IS DISTINCT FROM sqlc.narg(length_ms)
    OR beatport_updated IS DISTINCT FROM TRUE
  );

-- name: UpdateSongWithStmpdLinks :execrows
-- This no longer touches beatport_updated. That flag is the beatport fetcher's own
-- "already enriched, skip" sentinel; setting it here made the STMPD backfill skip
-- every row beatport had ever touched, which is why 497 beatport rows had no links.
-- stmpd_synced_at is this fetcher's separate record of having visited the row.
UPDATE songs SET
    spotify_url     = COALESCE(sqlc.narg(spotify_url),     spotify_url),
    apple_music_url = COALESCE(sqlc.narg(apple_music_url), apple_music_url),
    youtube_url     = COALESCE(sqlc.narg(youtube_url),     youtube_url),
    thumbnail_url   = CASE WHEN thumbnail_url IS NULL OR thumbnail_url = ''
                           THEN sqlc.narg(thumbnail_url) ELSE thumbnail_url END,
    stmpd_synced_at = NOW()
WHERE id = sqlc.arg(id)
  AND (
       spotify_url     IS DISTINCT FROM COALESCE(sqlc.narg(spotify_url),     spotify_url)
    OR apple_music_url IS DISTINCT FROM COALESCE(sqlc.narg(apple_music_url), apple_music_url)
    OR youtube_url     IS DISTINCT FROM COALESCE(sqlc.narg(youtube_url),     youtube_url)
    -- Only a reason to write when there is actually a thumbnail to write. Testing
    -- emptiness alone would match forever on a row neither side has artwork for,
    -- re-stamping it every cycle and reintroducing the churn this guard removes.
    --
    -- The cast is required, not cosmetic: IS NOT NULL accepts any type, so this is
    -- the one place the parameter appears without a type to infer from, and Postgres
    -- rejects the whole statement with 42P08 at execution time.
    OR (COALESCE(thumbnail_url, '') = '' AND sqlc.narg(thumbnail_url)::text IS NOT NULL)
    OR stmpd_synced_at IS NULL
  );

-- name: MarkBeatportUpdated :exec
UPDATE songs SET beatport_updated = TRUE WHERE id = $1;

-- name: GetAllSongsForMatching :many
SELECT id, name, artists, source, beatport_id, beatport_release_id,
       stmpd_slug, match_key, base_key, mix_name, spotify_url, stmpd_synced_at
FROM songs;

-- name: SetSongKeys :execrows
-- search_text rides along with the match keys rather than getting a writer of its own:
-- all three are derived from name + artists by the same pass, and a path that could
-- update one without the others is a path that leaves a renamed song unfindable.
--
-- Deliberately not guarded by locked_fields. These are pure derivations, not values a
-- human would ever want to pin; locking them would mean a hand-corrected title never
-- became searchable under its new spelling.
UPDATE songs SET match_key = $2, base_key = $3, search_text = $4
WHERE id = $1 AND (match_key IS DISTINCT FROM $2
                OR base_key IS DISTINCT FROM $3
                OR search_text IS DISTINCT FROM $4);

-- name: GetSongsForKeying :many
-- release_name is selected because search_text folds it in: it is how someone looks
-- for a track by the EP it came on, which is often all they remember of it.
SELECT id, name, artists, mix_name, length_ms, is_collection, apple_music_url, stmpd_slug,
       normalized_name, release_name, search_text
FROM songs ORDER BY id;

-- name: GetRandomSongForRadio :one
-- Canonical rows only, so the rotation does not play six versions of one track, and
-- no collections: an EP, an album or a remix package is a release containing songs,
-- not a song, and queueing one asks the player to stream a whole record as a track.
SELECT id, name, artists, thumbnail_url, youtube_url
FROM songs
WHERE youtube_url IS NOT NULL
  AND parent_song_id IS NULL
  AND NOT is_collection
  AND (length_ms IS NULL OR length_ms <= 600000)
ORDER BY RANDOM()
LIMIT 1;
-- name: MarkSongAnnounced :exec
UPDATE songs SET announced_at = NOW() WHERE id = $1 AND announced_at IS NULL;

-- name: GetSongsNeverAnnounced :many
SELECT id, name, artists, release_date FROM songs WHERE announced_at IS NULL;

-- name: MergeSongRows :exec
-- Fold `loser` into `winner`, keeping the first non-null of each field, and delete
-- the loser. Used when a release_date correction would collide with the
-- unique_release constraint: the collision means a twin row already holds that
-- identity, so the two are the same song arriving from two sources.
--
-- The delete is part of this statement rather than a DeleteSong call after it, and
-- that is the whole point. unique_release is (name, artists, mix_name, release_date)
-- NULLS NOT DISTINCT, and the COALESCEs below can hand the winner the very identity
-- the loser still holds: a pair differing only in that one names "Extended Mix" and
-- the other names no rendition ends with the winner adopting "Extended Mix" while the
-- loser is still sitting in the index. That is a 23505, and it took the whole merge
-- down after the release/adopt dance had already run --
--   Key (name, artists, mix_name, release_date)=(Waiting For Love, Brooks, Alida,
--   Extended Mix, 2019-07-26) already exists
-- -- on a catalogue where exactly that pair is the commonest kind of duplicate,
-- because beatport files nearly every plain release as an extended mix.
--
-- Deleting inside the statement removes the conflicting row before the index check
-- for the update runs, so the winner can take an identity the loser was holding.
-- Every caller deleted the loser on the next line anyway; none may do so now.
WITH l AS (
    DELETE FROM songs d WHERE d.id = sqlc.arg(loser_id) RETURNING d.*
)
UPDATE songs w SET
    spotify_url     = COALESCE(w.spotify_url,     l.spotify_url),
    apple_music_url = COALESCE(w.apple_music_url, l.apple_music_url),
    youtube_url     = COALESCE(w.youtube_url,     l.youtube_url),
    thumbnail_url   = COALESCE(NULLIF(w.thumbnail_url, ''), l.thumbnail_url),
    lyrics          = COALESCE(w.lyrics,          l.lyrics),
    bpm             = COALESCE(w.bpm,             l.bpm),
    musical_key     = COALESCE(w.musical_key,     l.musical_key),
    length_ms       = COALESCE(w.length_ms,       l.length_ms),
    genre           = COALESCE(w.genre,           l.genre),
    sub_genre       = COALESCE(w.sub_genre,       l.sub_genre),
    mix_name        = COALESCE(w.mix_name,        l.mix_name),
    release_name    = COALESCE(w.release_name,    l.release_name),
    -- The columns below were added after this query was first written. Leaving them
    -- out silently discarded the loser's slug and its deezer/tidal/amazon links.
    -- beatport_id and stmpd_slug are handled by Release/AdoptSongIdentifiers:
    -- both are uniquely indexed and cannot be copied while the loser still holds them.
    youtube_music_url   = COALESCE(w.youtube_music_url,   l.youtube_music_url),
    deezer_url          = COALESCE(w.deezer_url,          l.deezer_url),
    tidal_url           = COALESCE(w.tidal_url,           l.tidal_url),
    amazon_music_url    = COALESCE(w.amazon_music_url,    l.amazon_music_url),
    beatport_url        = COALESCE(w.beatport_url,        l.beatport_url),
    beatport_release_id = COALESCE(w.beatport_release_id, l.beatport_release_id),
    -- beatport_slug travels with beatport_id, and leaving it behind is not cosmetic: a
    -- track page is /track/<slug>/<id>, so a winner that adopts the loser's id without
    -- its slug ends up with a Beatport button that 404s. It was omitted here because
    -- the two uniquely-indexed identifiers next to it need the release/adopt dance;
    -- this one has no unique index and can simply be copied. A dedupe run that folded
    -- 108 beatport rows into STMPD winners broke 99 buttons that way.
    beatport_slug       = COALESCE(w.beatport_slug,       l.beatport_slug),
    -- A real date always beats the 1970-01-01 placeholder, whichever row holds it.
    release_date    = CASE WHEN w.release_date = '1970-01-01' AND l.release_date <> '1970-01-01'
                           THEN l.release_date ELSE w.release_date END,
    announced_at    = LEAST(w.announced_at,       l.announced_at),
    first_seen_at   = LEAST(w.first_seen_at,      l.first_seen_at),
    stmpd_synced_at = COALESCE(w.stmpd_synced_at, l.stmpd_synced_at),
    beatport_updated = w.beatport_updated OR l.beatport_updated,
    -- The union, not the winner's set. A lock records that a person corrected that
    -- column by hand; dropping the loser's locks would hand its hand-corrected columns
    -- straight back to the next sync to overwrite.
    locked_fields = (SELECT COALESCE(array_agg(DISTINCT e), '{}')
                     FROM unnest(w.locked_fields || l.locked_fields) e)
FROM l
WHERE w.id = sqlc.arg(winner_id);

-- name: DeleteSong :exec
DELETE FROM songs WHERE id = $1;

-- name: GetSongsMissingLinks :many
-- The backfill queue: rows the STMPD sync has never successfully applied to. After
-- migration 000009 this is exactly the set of rows carrying no streaming links.
SELECT id, name, artists, release_date, source, beatport_id
FROM songs WHERE stmpd_synced_at IS NULL ORDER BY id;

-- name: CountLinklessSongs :one
SELECT count(*) FROM songs
WHERE spotify_url IS NULL AND apple_music_url IS NULL AND youtube_url IS NULL;

-- name: SetSongParent :execrows
UPDATE songs SET parent_song_id = sqlc.narg(parent_song_id)
WHERE id = sqlc.arg(id) AND parent_song_id IS DISTINCT FROM sqlc.narg(parent_song_id)
  AND NOT ('parent_song_id' = ANY(locked_fields));

-- name: SetSongInstrumental :execrows
UPDATE songs SET is_instrumental = $2
WHERE id = $1 AND is_instrumental IS DISTINCT FROM $2
  AND NOT ('is_instrumental' = ANY(locked_fields));

-- name: GetSongsForParentLinking :many
-- stmpd_slug is selected because electing a canonical row weighs provenance first, and
-- a column that is not in the row cannot be weighed. Leaving it out is what let a
-- beatport row with no links win over the STMPD row for the same song.
SELECT id, name, artists, mix_name, release_date, source, base_key, stmpd_slug,
       spotify_url, youtube_url, apple_music_url, lyrics, parent_song_id, is_collection,
       locked_fields
FROM songs ORDER BY id;

-- name: GetSongMixes :many
-- The renditions hanging off a canonical song, for the track card and the dashboard's
-- song page to list.
--
-- The link and lyric columns are selected so a reader can see at a glance that a
-- rendition carries something its canonical row does not -- which is exactly the
-- defect that hid "Break Through The Silence"'s streaming links behind a linkless
-- beatport row, and is the button that promotes the better row.
SELECT id, name, mix_name, artists, release_date, thumbnail_url, source,
       spotify_url, youtube_url, apple_music_url, beatport_url,
       (lyrics IS NOT NULL)::boolean AS has_lyrics
FROM songs
WHERE parent_song_id = $1 ORDER BY release_date, id;

-- name: CopyLyricsToRemixes :execrows
-- Lyrics are entered by hand against the canonical row. A remix of a vocal track has
-- the same words, so fan them out rather than making someone paste them ten times.
--
-- lrclib_id travels with the words, and must. It is what separates lyrics that came
-- from LRCLIB from lyrics that exist nowhere else, and two things read that
-- distinction: an automatic fill is reversible only if every row it wrote can be
-- found, and dedupe-songs ranks a row carrying hand-entered lyrics above one without
-- when it picks which duplicate survives. Copying the words but not their provenance
-- made every fanned-out remix look hand-entered -- 146 of them on the first
-- production run.
--
-- A parent whose lyrics really were typed in has a NULL id, and the copy inherits
-- that, which is correct: it is a copy of something irreplaceable.
UPDATE songs t SET lyrics = s.lyrics, lrclib_id = s.lrclib_id
FROM songs s
WHERE s.id = $1 AND t.parent_song_id = s.id AND t.lyrics IS NULL
  AND s.lyrics IS NOT NULL AND NOT t.is_instrumental;

-- name: GetSongsWithPlaceholderDate :many
-- Rows whose release date is not a real date: the 1970-01-01 sentinel the original
-- importer wrote when it had none, and rows dated the 1st of January, which is what
-- the year-only scrape produced before the dataset gave exact dates.
-- Ordered so the ones resolvable from a stored link come first.
SELECT id, name, artists, release_date, apple_music_url, spotify_url
FROM songs WHERE release_date = '1970-01-01' OR release_date LIKE '%-01-01'
ORDER BY (apple_music_url IS NULL), id;

-- name: SetSongReleaseDate :execrows
UPDATE songs SET release_date = $2
WHERE id = $1 AND release_date IS DISTINCT FROM $2
  AND NOT ('release_date' = ANY(locked_fields));

-- name: GetDuplicateMatchKeyRows :many
-- Every row belonging to a match_key held by more than one row. A match_key is the
-- artist set, the base title and the rendition, so two rows sharing one are the same
-- recording stored twice -- usually once per source, with the variant in `name` on
-- one side and in `mix_name` on the other.
SELECT id, name, artists, mix_name, release_date, source, match_key,
       stmpd_slug, beatport_id, spotify_url, apple_music_url, youtube_url,
       lyrics, parent_song_id, thumbnail_url
FROM songs
WHERE match_key IN (
    SELECT match_key FROM songs
    WHERE match_key IS NOT NULL AND match_key <> '||'
    GROUP BY match_key HAVING count(*) > 1
)
ORDER BY match_key, id;

-- name: RepointChildren :execrows
-- Move a merged-away row's remixes onto the row that survives.
UPDATE songs SET parent_song_id = sqlc.arg(new_parent)
WHERE parent_song_id = sqlc.arg(old_parent);

-- name: ReleaseSongIdentifiers :exec
-- Clear the uniquely-indexed identifiers from a row that is about to be merged away.
--
-- beatport_id and stmpd_slug each carry a partial unique index, so copying them onto
-- the surviving row while this one still holds them violates the index. The caller
-- captured the values first and reassigns them with AdoptSongIdentifiers once this
-- row no longer claims them.
UPDATE songs SET beatport_id = NULL, stmpd_slug = NULL WHERE id = $1;

-- name: AdoptSongIdentifiers :exec
-- Give the surviving row the identifiers released above, without overwriting any it
-- already has of its own.
UPDATE songs SET
    beatport_id = COALESCE(beatport_id, sqlc.narg(beatport_id)),
    stmpd_slug  = COALESCE(stmpd_slug,  sqlc.narg(stmpd_slug))
WHERE id = sqlc.arg(id);

-- name: GetCanonicalSongsForReview :many
-- Songs as a user sees them, for reporting likely-duplicate groups that are not safe
-- to merge automatically.
SELECT id, name, artists, release_date, source, base_key, match_key,
       spotify_url, youtube_url, apple_music_url, beatport_id
FROM songs WHERE parent_song_id IS NULL AND base_key IS NOT NULL AND base_key <> '|'
ORDER BY base_key, id;

-- name: MarkSongUnreleased :exec
-- For adding a track that has been played but not put out. The date must go with it:
-- the unreleased_has_no_date constraint will not allow one without the other.
UPDATE songs SET is_unreleased = TRUE, release_date = NULL WHERE id = $1;

-- name: GetUnreleasedSongs :many
SELECT id, name, artists, lyrics IS NOT NULL AS has_lyrics, first_seen_at
FROM songs WHERE is_unreleased ORDER BY first_seen_at DESC, id;

-- name: SetSongCollection :execrows
UPDATE songs SET is_collection = $2
WHERE id = $1 AND is_collection IS DISTINCT FROM $2
  AND NOT ('is_collection' = ANY(locked_fields));

-- name: ClearStmpdSlug :execrows
-- Detach a release identity from a row it does not belong to.
UPDATE songs SET stmpd_slug = NULL WHERE id = $1;

-- name: GetSongsNeedingYoutube :many
-- Rows whose YouTube button is missing or points at a playlist rather than a video.
-- A playlist link is worse than none: it sends people to a playlist instead of the
-- song they asked for.
-- youtu.be short links are perfectly good videos -- the radio already follows them
-- -- so they are not "missing". Only a genuine playlist or an absent link is.
SELECT id, name, artists, mix_name, youtube_url, spotify_url
FROM songs
WHERE youtube_url IS NULL
   OR (youtube_url NOT LIKE '%watch?v=%' AND youtube_url NOT LIKE '%youtu.be/%')
ORDER BY id;

-- name: GetSongsWithUntidyYoutubeURL :many
-- Rows whose link is a video but not in canonical form: a short link, or one carrying
-- share tracking. Rewriting them makes every query and report agree on what a video
-- looks like.
SELECT id, youtube_url FROM songs
WHERE youtube_url IS NOT NULL
  AND (youtube_url LIKE '%youtu.be/%' OR youtube_url LIKE '%si=%' OR youtube_url LIKE '%&t=%')
ORDER BY id;

-- name: SetSongYoutubeURL :execrows
UPDATE songs SET youtube_url = sqlc.narg(youtube_url)
WHERE id = sqlc.arg(id) AND youtube_url IS DISTINCT FROM sqlc.narg(youtube_url)
  AND NOT ('youtube_url' = ANY(locked_fields));

-- name: ClearPlaylistSpotifyLinks :execrows
-- Spotify playlist links cannot be resolved without the Spotify API, and pointing a
-- song's Spotify button at a playlist is worse than showing no button.
UPDATE songs SET spotify_url = NULL WHERE spotify_url LIKE '%/playlist/%';

-- name: GetSongsMissingArtwork :many
-- Rows with no cover art. Beatport cannot help with these: every row it knows about
-- already has artwork, and none of these carry a beatport_id. Apple can -- most of
-- them have an Apple link, and the lookup returns a cover.
SELECT id, name, artists, apple_music_url
FROM songs WHERE coalesce(thumbnail_url, '') = ''
ORDER BY (apple_music_url IS NULL), id;

-- name: SetSongArtwork :execrows
UPDATE songs SET thumbnail_url = sqlc.narg(thumbnail_url)
WHERE id = sqlc.arg(id) AND coalesce(thumbnail_url, '') = ''
  AND NOT ('thumbnail_url' = ANY(locked_fields));

-- name: GetSongsToCheckForCollection :many
-- Rows not yet known to be releases, that carry an Apple link we can ask about.
SELECT id, name, artists, apple_music_url
FROM songs
WHERE NOT is_collection AND apple_music_url IS NOT NULL
  AND apple_music_url NOT LIKE '%/playlist/%'
ORDER BY id;

-- name: ClearUnresolvedYoutubePlaylists :execrows
-- A YouTube link that names a playlist rather than a video buys nothing: the radio
-- ignores it and falls back to a search either way, and the button sends a listener
-- to a playlist they did not ask for. Once everything resolvable has been resolved,
-- what is left is better as no button at all.
UPDATE songs SET youtube_url = NULL
WHERE youtube_url IS NOT NULL
  AND youtube_url NOT LIKE '%watch?v=%'
  AND youtube_url NOT LIKE '%youtu.be/%';

-- name: SetSongTitle :execrows
-- normalized_name is written here too, and has to be: this query is how rekey-songs
-- rewrites a name, so leaving it out would guarantee the derived column is stale on
-- exactly the rows that needed correcting.
UPDATE songs SET
    name = CASE WHEN 'name' = ANY(locked_fields) THEN name ELSE sqlc.arg(name) END,
    mix_name = CASE WHEN 'mix_name' = ANY(locked_fields) THEN mix_name ELSE sqlc.narg(mix_name) END,
    normalized_name = CASE WHEN 'normalized_name' = ANY(locked_fields) THEN normalized_name
                           ELSE sqlc.narg(normalized_name) END
WHERE id = sqlc.arg(id)
  AND ((NOT ('name' = ANY(locked_fields)) AND name IS DISTINCT FROM sqlc.arg(name))
    OR (NOT ('mix_name' = ANY(locked_fields)) AND mix_name IS DISTINCT FROM sqlc.narg(mix_name))
    OR (NOT ('normalized_name' = ANY(locked_fields))
        AND normalized_name IS DISTINCT FROM sqlc.narg(normalized_name)));

-- name: SetSongNormalizedName :execrows
-- The answerable form of the title, derived in Go by utils/title.go.
UPDATE songs SET normalized_name = sqlc.narg(normalized_name)
WHERE id = sqlc.arg(id) AND normalized_name IS DISTINCT FROM sqlc.narg(normalized_name)
  AND NOT ('normalized_name' = ANY(locked_fields));

-- name: GetSongsForSubsetDedupe :many
-- Everything needed to spot rows that are the same recording credited to different
-- subsets of the same artists.
SELECT id, name, artists, mix_name, release_date, source, stmpd_slug, beatport_id,
       spotify_url, apple_music_url, youtube_url, lyrics, parent_song_id, is_collection
FROM songs ORDER BY id;

-- name: SetBeatportSlug :execrows
-- Fills the slug that turns a stored beatport_id into a URL that resolves.
UPDATE songs SET beatport_slug = $2
WHERE id = $1 AND beatport_slug IS DISTINCT FROM $2;

-- name: GetSongsMissingBeatportSlug :many
-- Rows carrying a Beatport track id whose page URL cannot be built yet.
SELECT id, name, artists, mix_name, beatport_id FROM songs
WHERE beatport_id IS NOT NULL AND beatport_slug IS NULL ORDER BY id;

-- name: GetSongsForAudit :many
-- Everything the invariant checker needs to recompute a row's derived state and
-- compare it against what is stored.
SELECT id, name, artists, mix_name, length_ms, is_collection, parent_song_id,
       match_key, base_key, normalized_name, search_text, release_date,
       apple_music_url, spotify_url, youtube_url, stmpd_slug,
       beatport_id, beatport_slug, beatport_url, deezer_url, tidal_url,
       amazon_music_url, youtube_music_url, source,
       thumbnail_url, is_instrumental, is_unreleased, locked_fields, release_name,
       (lyrics IS NOT NULL)::boolean AS has_lyrics
FROM songs ORDER BY id;

-- name: GetSongsMissingLyrics :many
-- The LRCLIB backlog.
--
-- Canonical rows only: a rendition inherits its parent's words through
-- CopyLyricsToRemixes, so asking LRCLIB separately about each of a song's twelve
-- remixes is twelve times the requests for one answer.
--
-- The retry schedule widens with every miss -- 7 days, 28, 112, 448 -- and gives up
-- after four. LRCLIB is community-contributed and does grow, so "never again" is
-- wrong; but a row that has come up empty four times over a year and a half is not
-- going to be filled by asking a fifth.
--
-- Newest first among never-asked rows, because a song released this month is the one
-- people will actually be quizzed on and the one most likely to have just been added
-- upstream.
SELECT id, name, artists, mix_name, normalized_name, release_name, length_ms, lrclib_misses
FROM songs
WHERE lyrics IS NULL
  AND NOT is_instrumental
  AND NOT is_collection
  AND parent_song_id IS NULL
  AND lrclib_misses < 4
  AND (lrclib_checked_at IS NULL
       OR lrclib_checked_at < NOW() - (INTERVAL '7 days' * POWER(4, lrclib_misses)))
ORDER BY lrclib_checked_at NULLS FIRST, release_date DESC NULLS LAST, id
LIMIT sqlc.arg(lim);

-- name: SetSongLyrics :execrows
-- Guarded on lyrics IS NULL so a backfill can never overwrite words entered by hand.
-- Those exist nowhere else, which is why dedupe-songs makes them a winner-selection
-- tiebreaker, and a fill that clobbered them would be unrecoverable.
UPDATE songs SET lyrics = sqlc.arg(lyrics), lrclib_id = sqlc.narg(lrclib_id),
                 lrclib_checked_at = NOW(), lrclib_misses = 0
WHERE id = sqlc.arg(id) AND lyrics IS NULL
  AND NOT ('lyrics' = ANY(locked_fields));

-- name: MarkLrclibMiss :exec
-- LRCLIB answered and had nothing usable. Spends one of the row's four attempts.
UPDATE songs SET lrclib_checked_at = NOW(), lrclib_misses = lrclib_misses + 1
WHERE id = $1;

-- name: MarkLrclibChecked :exec
-- Stamps the attempt without spending one, for a row whose result was rejected for a
-- reason that is not LRCLIB's fault.
UPDATE songs SET lrclib_checked_at = NOW() WHERE id = $1;

-- name: MarkSongInstrumentalFromLrclib :execrows
-- LRCLIB says this recording has no words. Migration 000012 added is_instrumental for
-- exactly this: without a way to say so, an instrumental sits in the missing-lyrics
-- backlog forever and the quiz can pick it and ask a player to recall words that do
-- not exist. LRCLIB is the first source that can answer the question automatically.
UPDATE songs SET is_instrumental = TRUE, lrclib_id = sqlc.narg(lrclib_id),
                 lrclib_checked_at = NOW(), lrclib_misses = 0
WHERE id = sqlc.arg(id) AND NOT is_instrumental
  AND NOT ('is_instrumental' = ANY(locked_fields));

-- name: GetSongsWithSharedArtwork :many
-- Rows wearing a cover that also sits on a different song.
--
-- Sharing is counted per rendition family rather than per row: a song and its own
-- remixes share a cover legitimately, and they do not share a base key, because beatport
-- credits a remix to the original artists plus the remixer. Grouping on the family root
-- -- the parent id, or the row's own id when it is canonical -- is what tells "one
-- single's artwork across its versions" from "one compilation's cover across its
-- tracks".
SELECT id, name, artists, mix_name, release_name, thumbnail_url, source,
       apple_music_url, locked_fields
FROM songs
WHERE COALESCE(thumbnail_url, '') <> ''
  AND thumbnail_url IN (
      SELECT thumbnail_url FROM songs
      WHERE COALESCE(thumbnail_url, '') <> ''
      GROUP BY thumbnail_url
      HAVING count(DISTINCT COALESCE(parent_song_id, id)) > 1)
ORDER BY thumbnail_url, id;

-- name: ClearSongArtwork :execrows
-- Removes a cover that belongs to a different song, so that the Apple enrichment can
-- resolve the right one on its next pass.
--
-- SetSongArtwork cannot do this: it is guarded on the column being empty, because its
-- job is filling an absence and it must never overwrite what is already there. This one
-- exists precisely to overwrite -- but only a value no human has claimed.
UPDATE songs SET thumbnail_url = NULL
WHERE id = $1
  AND thumbnail_url IS NOT NULL
  AND NOT ('thumbnail_url' = ANY(locked_fields));
