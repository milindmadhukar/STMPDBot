package listeners

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// p is a local generic pointer helper: the ptr in profile_logs_test.go takes
// only strings, and attachments are a mix of int, float and string pointers.
func p[T any](v T) *T { return &v }

func TestMediaOf(t *testing.T) {
	t.Parallel()

	t.Run("an upload carries its type, alt text and voice duration", func(t *testing.T) {
		t.Parallel()

		got := mediaOf(discord.Message{Attachments: []discord.Attachment{
			{
				URL: "https://cdn.discordapp.com/a.png", Filename: "a.png", Size: 1234,
				ContentType: p("image/png"), Description: p("the setlist"),
				Width: p(800), Height: p(600),
			},
			{
				URL: "https://cdn.discordapp.com/v.ogg", Filename: "v.ogg",
				ContentType: p("audio/ogg"), DurationSecs: p(14.2),
			},
		}})

		if len(got) != 2 {
			t.Fatalf("got %d attachments, want 2", len(got))
		}
		if got[0].ContentType != "image/png" || got[0].Description != "the setlist" || got[0].Width != 800 {
			t.Errorf("image attachment lost detail: %+v", got[0])
		}
		if got[1].DurationSecs != 14.2 {
			t.Errorf("voice duration = %v, want 14.2 -- it is the only clue to how long the note is", got[1].DurationSecs)
		}
	})

	t.Run("a sticker becomes a fetchable CDN image", func(t *testing.T) {
		t.Parallel()

		got := mediaOf(discord.Message{StickerItems: []discord.MessageSticker{
			{ID: snowflake.ID(749054660769218631), Name: "wumpus", FormatType: discord.StickerFormatTypePNG},
		}})

		if len(got) != 1 {
			t.Fatalf("got %d, want the sticker", len(got))
		}
		if !strings.HasSuffix(got[0].URL, "/stickers/749054660769218631.png") {
			t.Errorf("sticker URL = %q", got[0].URL)
		}
		if got[0].Kind != "sticker" || got[0].ContentType != "image/png" {
			t.Errorf("sticker not labelled: %+v", got[0])
		}
	})

	t.Run("a lottie sticker is reported as the JSON it is", func(t *testing.T) {
		t.Parallel()

		got := mediaOf(discord.Message{StickerItems: []discord.MessageSticker{
			{ID: snowflake.ID(1), Name: "bouncy", FormatType: discord.StickerFormatTypeLottie},
		}})
		if got[0].ContentType != "application/json" {
			t.Errorf("content type = %q; a lottie is a vector animation, not an image", got[0].ContentType)
		}
	})

	t.Run("a link preview's picture counts as something seen", func(t *testing.T) {
		t.Parallel()

		got := mediaOf(discord.Message{Embeds: []discord.Embed{{
			Title:     "Martin Garrix - Animals",
			Thumbnail: &discord.EmbedResource{URL: "https://i.ytimg.com/x.jpg", ContentType: "image/jpeg", Width: 480},
		}}})

		if len(got) != 1 {
			t.Fatalf("got %d, want the embed image", len(got))
		}
		if got[0].Kind != "embed" || got[0].Filename != "Martin Garrix - Animals" {
			t.Errorf("embed not described: %+v", got[0])
		}
	})

	t.Run("the full image is preferred over the thumbnail of the same thing", func(t *testing.T) {
		t.Parallel()

		got := mediaOf(discord.Message{Embeds: []discord.Embed{{
			Image:     &discord.EmbedResource{URL: "https://example.test/full.png"},
			Thumbnail: &discord.EmbedResource{URL: "https://example.test/thumb.png"},
		}}})

		if len(got) != 1 || got[0].URL != "https://example.test/full.png" {
			t.Errorf("got %+v, want only the full image", got)
		}
	})

	t.Run("one message cannot flood the agent", func(t *testing.T) {
		t.Parallel()

		var atts []discord.Attachment
		for range maxMediaPerMessage + 5 {
			atts = append(atts, discord.Attachment{URL: "https://example.test/x.png", ContentType: p("image/png")})
		}
		if got := mediaOf(discord.Message{Attachments: atts}); len(got) != maxMediaPerMessage {
			t.Errorf("got %d, want the cap of %d", len(got), maxMediaPerMessage)
		}
	})

	t.Run("a plain text message has no media", func(t *testing.T) {
		t.Parallel()

		if got := mediaOf(discord.Message{Content: "hello"}); len(got) != 0 {
			t.Errorf("got %+v, want nothing", got)
		}
	})
}

func TestRenderEmoji(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, in, want string }{
		{"static", "nice <:pepeSTMPD:12345> track", "nice :pepeSTMPD: track"},
		{"animated", "<a:dance:999> lets go", ":dance: lets go"},
		{"several", "<:a:1><:b:2>", ":a::b:"},
		{"left alone when there is nothing to do", "just words", "just words"},
		{"a plain colon is not an emoji", "10:30 tonight", "10:30 tonight"},
	} {
		if got := renderEmoji(tc.in); got != tc.want {
			t.Errorf("%s: renderEmoji(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Discord expires the typing indicator after about ten seconds. A reply here
// routinely takes longer than that -- memory search, a tool loop, a model
// round-trip -- so sending it once left the channel looking idle while the bot
// was still working, and the person asking assumed it had broken.
func TestKeepTypingRefreshesUntilStopped(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var sends int

	// The real one talks to Discord; this exercises the loop shape that
	// matters -- an immediate send, then repeats until stopped.
	send := func() {
		mu.Lock()
		defer mu.Unlock()
		sends++
	}

	done := make(chan struct{})
	send()
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				send()
			}
		}
	}()
	stop := sync.OnceFunc(func() { close(done) })

	time.Sleep(120 * time.Millisecond)
	stop()
	stop() // must be safe twice: respond() defers it and may also return early

	mu.Lock()
	got := sends
	mu.Unlock()

	if got < 3 {
		t.Errorf("sent the indicator %d times; it has to be re-asserted or it expires mid-reply", got)
	}

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	after := sends
	mu.Unlock()
	if after != got {
		t.Errorf("kept sending after stop: %d -> %d", got, after)
	}
}
