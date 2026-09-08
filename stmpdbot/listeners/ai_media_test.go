package listeners

import (
	"strings"
	"testing"

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
