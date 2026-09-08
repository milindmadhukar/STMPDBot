# Maintenance scripts

One-off passes over the `songs` table. They are separate binaries rather than flags
on the bot for two reasons: a backfill rewrites release dates and deletes merged
duplicate rows, which is not something that should be reachable from a process that
runs unattended for weeks; and none of them import the notifier, so a maintenance run
announcing the back catalogue to Discord is structurally impossible rather than merely
guarded against.

Every script takes the same flags:

| flag | default | meaning |
|---|---|---|
| `-config` | `config.toml` | the bot's TOML config — the database credentials come from here, so there is no second copy to keep in step |
| `-dry-run` | `false` | report what would change, write nothing |
| `-timeout` | `30m` | overall deadline for the run |

## Order

`rekey-songs` populates the columns the others depend on. Run it first, and again
after any change to the normalization rules in `utils/matchkey.go` or
`utils/title.go`.

```
rekey-songs  →  backfill-stmpd  →  link-remix-parents  →  verify-catalogue
                import-beatport  ↗                      ↗
             →  backfill-lyrics  ↗
             →  fix-shared-artwork  →  backfill-artwork ↗
```

`backfill-lyrics` depends on `rekey-songs` for a different reason than the others:
LRCLIB is queried with `songs.normalized_name`, because it indexes song titles and
not credit strings. Asking it about "Sun Is Never Going Down (feat. Dawn Golden)"
finds nothing; asking about "Sun Is Never Going Down" finds the record.

`verify-catalogue` writes nothing and can be run at any time. Run it last: it names
the pass that repairs whatever it finds, so a non-empty report is a to-do list.

## Hand corrections and `locked_fields`

Every pass here writes columns a person can also edit in the dashboard, and these
processes run forever while a person types a value once. `songs.locked_fields` is what
stops the second from being undone by the first: editing a field in the dashboard adds
that column's name to the row's lock set, and **every query in this repo that an
automated path uses skips a column named there**.

That applies to the scripts as much as to the bot's tickers, so the "Writes:" lines
below should all be read as "…except columns listed in `songs.locked_fields`". A pass
that would have written a locked column reports it as unchanged, which is why a run can
legitimately report fewer writes than the rows it examined.

The derived columns are deliberately *not* lockable — `match_key`, `base_key`,
`search_text` and `normalized_name` are recomputed from the title and credits, and
pinning one would leave a hand-renamed song unfindable under the name it now displays.
`rekey-songs` therefore always rewrites them.

To hand a field back to automation, click its **locked** badge on the song's page in the
dashboard.

## The scripts

### `rekey-songs`

Recomputes the columns derived from a song's title. `match_key` and `base_key`
identify a recording (song + rendition + artist set) and a song (irrespective of
rendition). `search_text` is the folded haystack the catalogue is searched by -- the
credits, title, rendition and release name lowercased with accents stripped, apostrophes
dropped and punctuation collapsed -- which is what lets "matisse sadko dont tell me"
find a row credited to "Matisse & Sadko, Aspyer, Matluck". `normalized_name` is the
*answerable* form of the title — the name with
any rendition and any featured-artist credit removed, so the quiz can accept "Breach"
for a row stored as "Breach (Walk Alone)". All three are derived in Go, so a migration
cannot fill them in.

Note that `normalized_name` strips more aggressively than the passes that rewrite
`name` itself: those only drop a feature clause when the artists column already
credits those people, and the rows this exists for are exactly the ones where it does
not — "Sun Is Never Going Down (feat. Dawn Golden)" is credited to Martin Garrix
alone.

- **Requires:** migrations `000011_add_match_keys` and `000021_normalized_song_name`
- **Idempotent:** yes — rows whose values already match are not written
- **Writes:** `match_key`, `base_key`, `search_text`, `normalized_name`,
  `is_collection`, and occasionally `name`/`mix_name`

### `backfill-stmpd`

Walks the full STMPD RCRDS catalogue (~1015 releases, read from the label's publicly
readable Sanity dataset) and reconciles it against the table.

- fills in streaming links, artwork and the STMPD slug on rows that already exist
- corrects `release_date` from a `<year>-01-01` placeholder to the exact date, but
  **only** on an exact-identity match — a fuzzy match is grounds to add links, never
  to rewrite a date
- merges a row into its twin when a date correction collides with `unique_release`,
  because that collision means both rows are the same song from two sources
- inserts catalogue releases the table has never held, stamped as already announced

- **Requires:** migrations through `000011`, and `rekey-songs`
- **Idempotent:** yes — a second run reports `rows_written=0`
- **Writes:** all link columns, `thumbnail_url` (only when empty), `stmpd_slug`,
  `release_date`, `stmpd_synced_at`; inserts and deletes rows
- **Riskiest script here.** Take a dump first and read the `-dry-run` output.

It logs a per-tier breakdown of how matches were resolved. Tiers marked `exact=true`
rest on a stable identifier; the rest are inference and only ever add links.

### `link-remix-parents`

Points every remix row at the canonical row for the same song, and flags
instrumentals.

Beatport lists each remix as its own track, so one song becomes many rows —
"Catharina" is six, "Told You So" is twelve. No row is deleted: each is a real
recording with its own beatport id, BPM and length. What changes is presentation.
`parent_song_id IS NULL` is what `/links` autocomplete, `/lyrics`, `/quiz` and the
radio rotation filter on, so the catalogue reads as one entry per song again.

Grouping is by title plus artist *containment*, not `base_key`: beatport credits a
remix to the original artist plus the remixer, so their artist sets differ by
construction and a `base_key` match would never fire.

- **Requires:** migration `000012_remix_parent`, and `rekey-songs`
- **Idempotent:** yes
- **Writes:** `parent_song_id`, `is_instrumental`

### `backfill-dates`

Replaces the `1970-01-01` placeholder release dates with real ones.

The placeholder is what the original importer wrote when it had no date, and it is
not cosmetic: `release_date` drives the announcement recency window, the "released"
footer on a track card, and any date ordering, so a row stuck at 1970 sorts to the
bottom of the catalogue forever.

Dates come from Apple's public lookup API, resolved from the numeric id already
embedded in each row's own `apple_music_url` — not from a search. The date therefore
belongs to the exact recording the row already links to, and no fuzzy matching is
involved. Rows with no Apple link are reported, never guessed at.

Paced at one request every 3 seconds, so a full pass takes a few minutes.

- **Requires:** nothing beyond the base schema
- **Idempotent:** yes — a second run resolves 0
- **Writes:** `release_date`; merges and deletes a row when the corrected date
  collides with an existing twin

Its summary distinguishes three kinds of unresolvable row: no Apple link at all, an
Apple link pointing at a *playlist* rather than a release (a data problem worth
fixing — those buttons send users to the wrong place), and a link Apple no longer
has a record for.

#### `-recheck`

The other half of the job, and the half that writes nothing. `-recheck` ignores the
placeholders and walks the rows that *do* have a date, comparing each against the
recording it links to, and reports the ones more than 30 days apart.

A missing date announces itself. A wrong one does not — it is a real date, in range,
and describes the wrong event. Beatport reports `publish_date`, the day a track
appeared on beatport, so a label re-delivering its back catalogue hands us a date
years after the record came out: Roy Gates' 2012 "Midnight Sun 2.0 (Martin Garrix
Remix)" was stored, and announced in the server, as a 2021 release. Twenty rows were
wrong that way before anyone looked, and every one of them had been sitting in front
of members on a track card.

`UpdateSongWithBeatportData` no longer lets that happen — the enrichment may only
move a `release_date` *earlier* now, so a re-delivery cannot age a record forward.
This pass is what finds the ones that were already wrong, and anything a future
source gets wrong in the other direction.

It reports rather than corrects because Apple's date belongs to whichever release the
row happens to link to. A remix row pointed at the original single, or a track pointed
at the compilation it was later collected on, disagrees for a reason that is not "the
stored date is wrong", and rewriting those automatically would replace a handful of
wrong dates with a hundred. Each finding therefore carries what Apple calls the
linked recording and which release it sits on, plus `link_is_this_song=false` when the
row's Apple link points at a different recording entirely — that is a broken link
being reported, and the date is only its symptom.

Correct a date on the song's dashboard page rather than in SQL: that locks
`release_date`, which both keeps every pass off it and takes the row out of this
report on the next run.

- **Requires:** nothing beyond the base schema
- **Idempotent:** trivially — it writes nothing
- **Writes:** nothing

### `dedupe-songs`

Folds together rows that represent the same recording, identified by an exact
`match_key` (artist set + base title + rendition).

They exist for two reasons: the two sources disagree about where a rendition belongs
— STMPD publishes "Grove (Rework)" as a title while beatport files "Grove" with
`mix_name` "Rework" — and for months the old matcher could not pair rows across
sources at all, so the beatport importer inserted its own copy of songs already
present. The symptom is one song offering two identical-looking `/links` entries.

**Not every duplicate is safe to merge blind.** Where two rows disagree on the release
date, one date is about to be discarded, and a wide gap can mean a genuine re-release.
Merging is therefore bounded by `-max-date-gap` (default 120 days, which covers the
normal disagreement between beatport's publish date and STMPD's release date);
anything wider is logged for a human and left untouched.

The surviving row is chosen by provenance: an STMPD slug first, then hand-entered
lyrics (they exist nowhere else and must not be what disappears), then streaming
links, then the earliest real date — a `1970-01-01` placeholder never wins on date.

- **Requires:** `rekey-songs`
- **Idempotent:** yes
- **Writes:** merges and deletes rows; repoints any remixes onto the surviving row

`-report-suspects=out.csv` writes the duplicates an exact `match_key` *cannot* catch
and exits without changing anything. These are rows that agree on the title but
disagree on who made it, in three shapes needing different judgement:

| reason | example | note |
|---|---|---|
| `feature-omitted` | `Moksi Ft. Adam McInnis` vs `Moksi` | one side drops the featured artist |
| `artist-alias-or-corrupt` | `Duncan` vs `Düncan Musique` | same act, two spellings |
| `artist-alias-or-corrupt` | `Able HeartDetonate`, `rionosremixes` | the title bled into the artists field — fix the row, do not merge |

A shared title plus a subset credit is *not* proof of a duplicate, which is why this
reports rather than merges.

The run makes three passes, narrowest evidence first:

1. **exact `match_key`** — same artist set, same base title, same rendition
2. **subset credit** — same title and rendition, one artist set contained in the other
   (`Moksi` and `Moksi Ft. Adam McInnis`)
3. **shared identifier** — the two rows link to the same YouTube video, Apple release
   or Spotify record

The third exists because the first two both reason about the artist string, and a
renamed act defeats any such reasoning: `Sikdope & IBRA` and `Sikdope, Ibranovski` are
one release, as are `King Arthur`/`King Topher` and `DJ Raiden`/`Raiden`. Neither
artist set contains the other, and the substring test in the suspects report needs six
characters on both sides before it will accept one name as another — `ibra` is four.
Lowering that floor is not the fix, because `Ra` is a substring of `Raiden` and an
artist in its own right.

So the third pass reads evidence instead of credits: a row that links to YouTube
`NxhlqzCtc2w` *is* that recording, whatever it calls its artists. It merges only where
the base titles are equal and the renditions agree, which is what keeps a track and the
EP it appears on — they share a release id and often a video — from collapsing into
each other. Rows it declines to merge for any reason other than "different songs" are
logged with that reason.

### `import-beatport`

One-off unbounded import of the Beatport catalogue — the bot's former
`--fetch-all-beatport` flag. Inserts tracks the table does not already represent,
stamped as already announced.

- **Requires:** working Beatport credentials in the config, and `rekey-songs`
- **Idempotent:** yes — existing tracks resolve through the matcher and are skipped
- **Writes:** inserts rows with beatport metadata; fills `beatport_slug` on rows it
  already has
- Run `link-remix-parents` afterwards so the new remix rows are grouped.

It is also how Beatport links get fixed. A track page is `/track/<slug>/<id>` and the
catalogue only ever stored the id, so every Beatport button led to a 404. The slug
cannot be derived locally — Beatport slugifies the track's full name including the
feature credit that this catalogue strips into the artist column, so "X's feat. Icona
Pop" becomes `xs-feat-icona-pop` where the stored title would give `xs`. This walk has
the authoritative value in hand and stores it on rows it skips as well as rows it
inserts.

### `backfill-lyrics`

Fills `songs.lyrics` from [LRCLIB](https://lrclib.net), a free community lyrics
database that needs no key and no account.

Lyrics have only ever been entered by hand through `make psql`, so 79 rows out of 1348
have them — and that handful is the entire pool the `/quiz` command draws from. This is
the sweep that changes that. The bot's own daily watcher works the same queue in
batches of sixty, which would take weeks to reach the end of it.

Paced at one request every 500ms, which is the slower end of what LRCLIB's
documentation asks for; a full pass over ~1200 rows takes about twenty minutes. A `429`
stops the run rather than pushing through it — their docs say continuing may earn a
ban, and the schedule this writes means a second run resumes exactly where it stopped.

Every candidate is verified before its words are believed: `utils.SameRecording` for
title and artists, plus a five-second duration check, which is the only thing that
separates a song from its own cover, live cut or sped-up edit. **Run `-dry-run` first
and read the log.** Each fill prints the record it came from, and hanging the wrong
words on a song is the one failure here that nobody would ever notice.

Where LRCLIB reports a track as instrumental — and only from an exact lookup, never
from the search fallback — the row is flagged `is_instrumental` instead. That is what
migration `000012` added the column for: an instrumental with no words otherwise sits
in this backlog forever and the quiz can pick it and ask a player to recall lyrics that
do not exist.

Filling a canonical row fans the words out to its renditions via `CopyLyricsToRemixes`,
so a song's twelve remixes are not twelve separate lookups for the same answer.

- **Requires:** migration `000022_lrclib_lyrics`, and `rekey-songs` (see Order above)
- **Idempotent:** yes — a row with words is never asked about again, and one LRCLIB has
  nothing for is retired after four misses on a widening schedule (7 days, 28, 112, 448)
- **Writes:** `lyrics` (only where NULL — hand-entered words are never overwritten),
  `lrclib_id`, `lrclib_checked_at`, `lrclib_misses`, `is_instrumental`

### `backfill-artwork`

Fills in missing cover art from Apple, resolved from the numeric id already embedded in
each row's own `apple_music_url`. Where a row has no Apple link, the same verified
search used for release dates finds one; an unverified result would hang the wrong
artwork on the song.

Paced at one request every 3 seconds. It is the pass that repairs whatever
`fix-shared-artwork` clears, so run it after that one.

- **Requires:** nothing beyond the base schema
- **Idempotent:** yes — a row that has a cover is never asked about again
- **Writes:** `thumbnail_url` (only where empty, and never on a locked row)

### `fix-shared-artwork`

Removes cover art that belongs to a different song.

Beatport exposes no per-track image, only a per-release one, and the importer took it
unconditionally — so every track on a compilation came back wearing the compilation's
cover. One image sat on twelve unrelated songs, and 295 rows in production wore artwork
belonging to something else: a listener asking for "Dragon" got a card showing the
Tomorrowland 2016 sleeve.

A cover is cleared when the rows wearing it credit **nobody in common**, because then it
cannot be any of their artwork. That is the whole rule, and it is the same one
`verify-catalogue` reports, so a run of this pass and the report that sent you to it
cannot disagree about which rows are wrong.

Sharing on its own is not a defect, which is why the count is not the test:

- a song and its own renditions share a cover — that is what a single's artwork is;
- the tracks of one act's own EP share a cover — "Front 2 Back" is Bart B More on "The
  Street EP", and the EP sleeve is exactly what Apple and Spotify show for it.

Judging by release name instead ("the release is not named after this track") looks
right and is not: it would have stripped 115 rows of correct EP artwork. That rule lives
in `utils.BeatportReleaseIsThisTrack` and belongs in the *importer*, where it stops new
bad rows arriving; here the artist-overlap rule is the correct one. It is also what
catches Beatport's placeholder images, which land on unrelated singles whose release
name **is** the track name.

This pass only clears. Refilling is `backfill-artwork`'s job, and running it afterwards
is the point of clearing: a NULL cover is a row the Apple enrichment will resolve, while
a wrong one is a row nothing will ever revisit.

- **Requires:** `rekey-songs` (for `base_key`) and migration `000024`
- **Idempotent:** yes — a second run clears 0
- **Writes:** `thumbnail_url` (only to NULL, and never on a locked row)

```
make fix_shared_artwork_dry     # read this first
make fix_shared_artwork
make backfill_artwork           # resolve replacements from Apple
```

### `verify-catalogue`

Read-only. Recomputes each row's derived state and reports every row that disagrees
with what is stored, grouped by invariant, naming the pass that fixes it.

- **Requires:** nothing; safe to run against production at any time
- **Writes:** nothing — it does not import a query that mutates
- **Exit:** always 0; read the `checks_failed` and `rows_flagged` summary

Checks: the collection flag against a recomputation, stale `match_key`/`base_key`,
stale `search_text`, stale `normalized_name`, canonical rows sharing a normalized name
with overlapping credits (one song written several ways — the catalogue holds "Now that
I've Found You Feat. \"John & Michel\"" and two other spellings of the same song, which
no match key can see because the credits differ), rows sharing a match key (the same
recording twice), rendition trees that are deeper than one level or rooted at a release
or at a row's own id, renditions left unfiled while their song sits in the table,
**canonical rows carrying less than one of their own renditions does**, **covers shared
by unrelated acts**, **songs with no cover at all**, Beatport ids with no slug to build
a URL from, tracking parameters on streaming links, and songs carrying no link at all.

The invariants themselves live in `utils/catalogue`, not in this command. The
dashboard's **Catalogue → What's wrong** page reports the same set by calling the same
`catalogue.Audit`, so the two cannot drift; what lives here is only how a terminal
renders them.

It exists because the alternative was reading the table by hand. Every defect found
that way turned out to be a class rather than a row — one wrongly flagged collection
was hiding twenty-seven songs from search — so the useful unit of work is the
invariant.

### `backfill-modlogs`

Imports historical moderation from Discord's audit log into `modlogs`, so the
dashboard's moderation page is not empty on the day the audit-log listener ships.

- **Requires:** the bot's role has **View Audit Log** in the guild; `-guild=<id>`
- **Writes:** `modlogs` rows carrying `audit_log_id`
- **Exit:** 0 on success; the summary reports `inserted` and `entries_seen`

```
go run ./scripts/backfill-modlogs -config=config.toml -guild=690950056202731521 -dry-run
go run ./scripts/backfill-modlogs -config=config.toml -guild=690950056202731521
```

Discord retains roughly 45 days of audit history, so this is a one-shot catch-up
rather than a full archive; everything after it is captured live by the listener.
Safe to re-run — rows are keyed on the audit entry ID and inserted with
`ON CONFLICT DO NOTHING`, so a second pass over the same window writes nothing.

Entries whose executor is the bot itself are skipped: the `/moderation` command
that performed them already wrote a row naming the human moderator, and importing
the audit entry too would double-count the action and attribute it to the bot.

## Running against production

The bot on `limitless` runs from `~/STMPDBot` against the shared
`postgres-db-1` container. The scripts are not in the deployed image (the Dockerfile
builds only the bot binary), and there is no Go toolchain on the server — so run them
from a local checkout. Postgres is published on the host's Tailscale address, so no
tunnel is needed:

```toml
# config.prod.toml — gitignore this, it holds the database password
[database]
host = "100.74.136.119"
user = "postgres"
password = "<the password from ~/STMPDBot/config.docker.toml>"
name = "garrixbot"
port = 5432
```

`import-beatport` additionally needs the `[bot]` beatport credentials from that same
file. The other three scripts only read `[database]`.

**Take a dump first.** `backfill-stmpd` deletes merged rows, and there is no undo:

```sh
ssh milind@100.74.136.119 \
  'docker exec postgres-db-1 pg_dump -U postgres -d garrixbot --no-owner' \
  > garrixbot-$(date +%F).sql
```

Then, in order:

```sh
go run ./scripts/rekey-songs         -config=config.prod.toml
go run ./scripts/backfill-stmpd      -config=config.prod.toml -dry-run   # read this
go run ./scripts/backfill-stmpd      -config=config.prod.toml
go run ./scripts/link-remix-parents  -config=config.prod.toml
go run ./scripts/fix-shared-artwork  -config=config.prod.toml -dry-run   # read this too
go run ./scripts/fix-shared-artwork  -config=config.prod.toml
go run ./scripts/backfill-artwork    -config=config.prod.toml
go run ./scripts/verify-catalogue    -config=config.prod.toml
```

`rekey-songs` must run before the dashboard is useful: `search_text` is NULL on every
row until it does, and search falls back to an unfolded expression that cannot find a
song by a partial credit string. Migration `000024` adds the column, so deploy the bot
image first.

Run `backfill-stmpd` until it reports `rows_written=0`. It converges in three or four
passes: each pass records which release owns which row, and a release displaced
earlier in a pass reclaims its row on the next one. Against the August 2026 catalogue
that sequence was 862 → 84 → 4 → 0.

Migrations run automatically when the bot boots, so deploy the new image before
running any of this — `rekey-songs` needs the columns from `000011` to exist.

## Verifying afterwards

```sql
-- Nothing can be announced: every row is stamped.
SELECT count(*) FROM songs WHERE announced_at IS NULL;              -- expect 0

-- Songs still carrying no streaming links, by source.
SELECT source, count(*) FROM songs
WHERE spotify_url IS NULL AND apple_music_url IS NULL AND youtube_url IS NULL
GROUP BY 1;

-- The catalogue as users see it.
SELECT count(*) FILTER (WHERE parent_song_id IS NULL) AS canonical,
       count(*) FILTER (WHERE parent_song_id IS NOT NULL) AS remixes FROM songs;
```

## import-mee6

Replaces this bot's `total_xp` and `messages_sent` with Mee6's, for one guild.

Mee6 ran alongside this bot in the STMPD server the whole time and kept counting
correctly while our own numbers drifted -- its XP total for the server was about
three and a half times ours, and our `messages_sent` was inflated for roughly half
the members. Both systems use the *same* level curve (`5*lvl^2 + 50*lvl + 100`,
which is `utils.FXpForNextLevel`, because the Python predecessor copied Mee6's),
so the numbers are directly comparable and nothing is converted.

```
go run ./scripts/import-mee6 -config config.prod.toml -guild <id> -dry-run
go run ./scripts/import-mee6 -config config.prod.toml -guild <id>
```

It **overwrites**: members whose Mee6 XP is lower than ours lose the difference.
Members absent from Mee6 are left alone rather than zeroed, and coins are never
touched.

Unlike the other passes, `-dry-run` here writes nothing at all instead of writing
inside a rolled-back transaction. That transaction holds a row lock on every
member for the length of the pass, and on the live database the bot's
`MessageSent` queued behind it for 21 seconds. The real run writes a row at a
time so each lock lasts about a millisecond.

Take a backup first -- `CREATE TABLE users_backup_pre_mee6 AS SELECT * FROM users;`
-- because the overwrite is not otherwise reversible.
