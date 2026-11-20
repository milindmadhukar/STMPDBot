package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// stmpdrcrds.com/archive is a Next.js front end over a Sanity CMS whose dataset is
// publicly readable. Querying the dataset directly replaces scraping the page:
//
//   - the page embeds only the first 48 releases; the dataset holds 1015
//   - the page's release objects carry a year the scrape had to re-derive; the
//     dataset carries the exact ISO date
//   - the page exposes spotify, appleMusic and youtube; the dataset also carries
//     youtubeMusic, deezer, tidal, amazonMusic and a beatport release URL
//   - a redesign breaks a payload scrape. It does not break a dataset query.
const (
	// stmpdUserAgent identifies the bot to both stmpdrcrds.com and the Sanity CDN.
	stmpdUserAgent = "STMPDBot (+https://github.com/milindmadhukar/STMPDBot)"

	sanityProjectID = "s5uw0tsi"
	sanityDataset   = "production"
	sanityAPIVer    = "v2021-10-21"
)

// sanityPageSize bounds one GROQ response. The full catalogue is roughly 2MB, which
// is fine for a one-off maintenance pass and wasteful every fifteen minutes.
const sanityPageSize = 200

// sanityGROQ projects the fields the bot stores.
//
// artworkUrl is projected from artwork.asset->url rather than read directly: the
// archive page appears to expose an artworkUrl field, but that is derived by the
// page's server and no such field exists on the documents themselves. Only about
// 208 of the 1015 releases have artwork at all, though every release since the
// start of 2025 does -- so callers must treat an empty artwork URL as "unknown",
// never as "this release has no artwork, clear what you have".
const sanityGROQ = `*[_type=="release" && defined(slug.current) && releaseDate >= $since] | order(releaseDate asc)[$from...$to]{
  title, artists, version, releaseDate,
  "slug": slug.current,
  "artworkUrl": artwork.asset->url,
  streamingLinks
}`

// SanityRelease is one release document as the dataset stores it.
type SanityRelease struct {
	Title       string `json:"title"`
	Artists     string `json:"artists"`
	Version     string `json:"version"`
	ReleaseDate string `json:"releaseDate"`
	Slug        string `json:"slug"`
	ArtworkURL  string `json:"artworkUrl"`

	StreamingLinks struct {
		Spotify      string `json:"spotify"`
		AppleMusic   string `json:"appleMusic"`
		YouTube      string `json:"youtube"`
		YouTubeMusic string `json:"youtubeMusic"`
		Deezer       string `json:"deezer"`
		Tidal        string `json:"tidal"`
		AmazonMusic  string `json:"amazonMusic"`
		Beatport     string `json:"beatport"`
	} `json:"streamingLinks"`
}

// Name renders the release the way songs.name stores it: the title, with the
// version in parentheses when there is one. Version is frequently absent or empty,
// and three separate "Repeat It" documents differ only by it.
func (r SanityRelease) Name() string {
	v := strings.TrimSpace(r.Version)
	if v == "" {
		return r.Title
	}
	return fmt.Sprintf("%s (%s)", r.Title, v)
}

// Artwork returns the artwork URL resized to the square the announcement embeds
// want, or "" when the release has none.
func (r SanityRelease) Artwork() string {
	if r.ArtworkURL == "" {
		return ""
	}
	return r.ArtworkURL + "?w=1000&h=1000&fit=crop&auto=format"
}

// CleanLink strips the tracking a share link carries.
//
// The catalogue hands out "open.spotify.com/album/xyz?si=<token>" and similar, and a
// one-off cleanup does not hold: the next sync writes the parameters straight back.
// Stripping happens on the way in, not afterwards.
//
// Apple's "i=" is the exception -- it names the individual track within a release, so
// it is part of the address rather than tracking.
func CleanLink(rawURL string) string {
	if rawURL == "" {
		return ""
	}

	q := strings.IndexByte(rawURL, '?')
	if q < 0 {
		return rawURL
	}

	base := rawURL[:q]
	if !strings.Contains(rawURL, "music.apple.com") && !strings.Contains(rawURL, "itunes.apple.com") {
		return base
	}

	for _, part := range strings.Split(rawURL[q+1:], "&") {
		if strings.HasPrefix(part, "i=") {
			return base + "?" + part
		}
	}
	return base
}

// Text wraps a possibly-empty string as a nullable column value. Empty means "the
// source does not know", which must read as NULL so that COALESCE in the update
// queries leaves whatever is already stored alone -- rather than erasing a link or a
// thumbnail that another source supplied.
func Text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

// BeatportReleaseID extracts the trailing numeric id from a beatport release URL,
// e.g. https://www.beatport.com/release/listen-up-extended-mix/7348516.
//
// This is a RELEASE id, not a track id, so it must never be written to
// songs.beatport_id -- that column holds track ids from the beatport API and the two
// namespaces overlap. Only about 30 of the 1015 dataset releases carry one, so it is
// a free exact match where present and never something to depend on.
func BeatportReleaseID(rawURL string) pgtype.Int4 {
	if rawURL == "" {
		return pgtype.Int4{}
	}

	trimmed := strings.TrimRight(rawURL, "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx < 0 {
		return pgtype.Int4{}
	}

	id, err := strconv.Atoi(trimmed[idx+1:])
	if err != nil || id <= 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(id), Valid: true}
}

// Query renders the release as everything that could identify an existing row.
func (r SanityRelease) Query() SongQuery {
	return SongQuery{
		Title:             r.Title,
		Version:           r.Version,
		Artists:           r.Artists,
		StmpdSlug:         r.Slug,
		SpotifyURL:        r.StreamingLinks.Spotify,
		BeatportReleaseID: BeatportReleaseID(r.StreamingLinks.Beatport),
		ReleaseDate:       r.ReleaseDate,
	}
}

// SanityClient reads the STMPD release dataset. The endpoint is public, so there
// is no authentication to hold.
type SanityClient struct {
	httpClient *http.Client
}

func NewSanityClient() *SanityClient {
	return &SanityClient{httpClient: &http.Client{Timeout: 30 * time.Second}}
}

type sanityResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Description string `json:"description"`
	} `json:"error"`
}

// query runs one GROQ query and decodes its result into out.
func (c *SanityClient) query(ctx context.Context, groq string, params map[string]any, out any) error {
	endpoint := fmt.Sprintf("https://%s.api.sanity.io/%s/data/query/%s",
		sanityProjectID, sanityAPIVer, sanityDataset)

	q := url.Values{}
	q.Set("query", groq)
	for name, v := range params {
		// GROQ parameters are passed as $<name> query-string entries holding JSON.
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("failed to encode groq param %q: %w", name, err)
		}
		q.Set("$"+name, string(encoded))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("failed to build sanity request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", stmpdUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sanity request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read sanity response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sanity returned status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var parsed sanityResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("failed to decode sanity response: %w", err)
	}
	if parsed.Error != nil {
		return fmt.Errorf("sanity query error: %s", parsed.Error.Description)
	}
	if err := json.Unmarshal(parsed.Result, out); err != nil {
		return fmt.Errorf("failed to decode sanity result: %w", err)
	}
	return nil
}

// FetchStmpdReleases returns every release published on or after since, oldest
// first. An empty since fetches the whole catalogue.
//
// Oldest-first matters: callers insert as they go and match later releases against
// what they have already inserted, so the original of a track needs to be in place
// before its remixes arrive.
func (c *SanityClient) FetchStmpdReleases(ctx context.Context, since string) ([]SanityRelease, error) {
	if since == "" {
		since = "0000-01-01"
	}

	var all []SanityRelease
	for from := 0; ; from += sanityPageSize {
		var page []SanityRelease
		params := map[string]any{"since": since, "from": from, "to": from + sanityPageSize}
		if err := c.query(ctx, sanityGROQ, params, &page); err != nil {
			return nil, err
		}

		all = append(all, page...)
		if len(page) < sanityPageSize {
			return all, nil
		}
	}
}

// SinceDaysAgo formats a lookback window as the ISO date GROQ compares against.
func SinceDaysAgo(days int) string {
	return time.Now().AddDate(0, 0, -days).Format(time.DateOnly)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
