package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/utils"
)

var imageRegex = regexp.MustCompile(`https://.*\.(?:jpg|jpeg|gif|png)`)

func AuthenticateReddit(b *stmpdbot.STMPDBot) error {
	data := url.Values{}
	data.Set("grant_type", "password")
	data.Set("username", b.Cfg.Bot.RedditBotUsername)
	data.Set("password", b.Cfg.Bot.RedditBotPassword)

	// Tighter than the client's own timeout: a token request is small, and a
	// slow one should give the cycle back rather than eat most of it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", "https://www.reddit.com/api/v1/access_token", strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("failed to create reddit auth request: %w", err)
	}
	req.SetBasicAuth(b.Cfg.Bot.RedditClientID, b.Cfg.Bot.RedditClientSecret)
	req.Header.Set("User-Agent", redditUserAgent(b))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := b.RedditClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("reddit API error: %s - %s", resp.Status, string(body))
	}

	// Parse response
	var redditToken utils.RedditToken
	if err := json.Unmarshal(body, &redditToken); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	redditToken.ExpiresAt = time.Now().Add(time.Duration(redditToken.ExpiresIn) * time.Second)

	b.RedditToken = redditToken

	return nil
}

// redditTokenRefreshMargin re-authenticates slightly ahead of the advertised
// expiry so a cycle can never start with a token that dies mid-request.
const redditTokenRefreshMargin = utils.TokenRefreshMargin

// redditUserAgent builds the User-Agent Reddit's API rules ask for
// (platform:app-id:version, plus the owning account). Generic agents are one of
// the things their edge blocker rejects outright.
func redditUserAgent(b *stmpdbot.STMPDBot) string {
	return fmt.Sprintf("linux:stmpdbot:v1.0 (by /u/%s)", b.Cfg.Bot.RedditBotUsername)
}

// ensureRedditToken refreshes the access token when it is missing or close to
// expiring. This runs on every cycle: Reddit's tokens last 24h, so authenticating
// only once at startup leaves a long-lived process silently unauthenticated.
func ensureRedditToken(b *stmpdbot.STMPDBot) error {
	if !utils.NeedsRefresh(b.RedditToken.AccessToken, b.RedditToken.ExpiresAt,
		time.Now(), redditTokenRefreshMargin) {
		return nil
	}

	slog.Info("Reddit token expired or not set, authenticating...")
	if err := AuthenticateReddit(b); err != nil {
		return fmt.Errorf("failed to authenticate reddit: %w", err)
	}

	if b.RedditToken.AccessToken == "" {
		return fmt.Errorf("reddit access token is empty after authentication")
	}

	return nil
}

// runRedditCycle performs a single fetch-and-announce pass. It is its own
// function so the response body is closed at the end of every cycle, rather than
// piling up deferred closes for the lifetime of the never-returning fetcher loop.
func runRedditCycle(b *stmpdbot.STMPDBot, endpoint string) error {
	if err := ensureRedditToken(b); err != nil {
		return err
	}

	notifier := utils.NewBatchNotifier(b.Queries, b.Client.Rest, utils.NotificationTypeReddit)

	req, err := http.NewRequest("GET", "https://oauth.reddit.com"+endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to create reddit request: %w", err)
	}
	req.Header.Set("User-Agent", redditUserAgent(b))
	req.Header.Set("Authorization", "bearer "+b.RedditToken.AccessToken)

	resp, err := b.RedditClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch reddit posts: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	// Reddit reports auth and rate-limit failures with a JSON body that decodes
	// cleanly into RedditResponse as zero posts, so the status has to be checked
	// explicitly. Without this every failure looks like "no new posts".
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// Drop the token so the next cycle re-authenticates: it may have been
			// revoked server-side before the expiry we recorded.
			b.RedditToken = utils.RedditToken{}
		}
		return fmt.Errorf("reddit API error: %s - %s", resp.Status, utils.CutString(string(bodyBytes), 512))
	}

	var data utils.RedditResponse
	if err = json.Unmarshal(bodyBytes, &data); err != nil {
		return fmt.Errorf("failed to decode reddit response: %w - body: %s", err, utils.CutString(string(bodyBytes), 512))
	}

	posts := data.Data.Children
	if len(posts) > 5 {
		posts = posts[:5]
	}
	slices.Reverse(posts)

	for _, post := range posts {
		err := b.Queries.InsertRedditPost(context.Background(), post.Data.ID)
		if err != nil {
			// Post already exists, skip it
			continue
		}

		redditPostEmbed := discord.NewEmbed().
			WithTitle(html.UnescapeString(utils.CutString(post.Data.Title, 256))).
			WithURL("https://www.reddit.com"+post.Data.Permalink).
			WithTimestamp(time.Unix(int64(post.Data.CreatedUtc), 0)).
			WithDescription(utils.CutString(html.UnescapeString(post.Data.Selftext), 2048)).
			WithFooter(fmt.Sprintf("Author u/%s on Subreddit %s", post.Data.Author, post.Data.SubredditNamePrefixed), "").
			// TODO: Change to reddit orange
			WithColor(utils.ColorSuccess)

		if imageRegex.MatchString(post.Data.URL) {
			redditPostEmbed.Image = &discord.EmbedResource{
				URL: post.Data.URL,
			}
		}

		// Add this post to the batch
		embed := redditPostEmbed
		notifier.AddItem(utils.NotificationItem{
			Embed: &embed,
		})
	}

	// Send all batched notifications once
	if err := notifier.Send(); err != nil {
		return fmt.Errorf("failed to send batched reddit notifications: %w", err)
	}

	return nil
}

func GetRedditPosts(b *stmpdbot.STMPDBot, ticker *time.Ticker) {
	endpoint := fmt.Sprintf("/r/Martingarrix/new?limit=%d", 5)

	for ; ; <-ticker.C {
		slog.Info("Running reddit post fetcher")

		// A failed cycle must never end the loop. An expired token, a rate limit
		// or a Reddit outage should cost one cycle, not every future one.
		if err := runRedditCycle(b, endpoint); err != nil {
			slog.Error("Reddit post fetch cycle failed", slog.Any("err", err))
			utils.RecordSourceFailure(utils.SourceReddit, err)
		} else {
			utils.RecordSourceSuccess(utils.SourceReddit)
		}
	}
} // TODO: Maybe some logic to restart if it panics?
