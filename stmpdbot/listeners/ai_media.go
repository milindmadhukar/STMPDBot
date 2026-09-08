package listeners

import (
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/milindmadhukar/STMPDBot/utils"
)

// This file is the bot's half of giving the persona senses: it reports what
// was posted alongside the words. It reports only -- no downloading, no
// deciding what is usable. Those belong to the agent service (stmpdbot/ai),
// which is the side that knows what the model can be shown.
//
// Three things count as media on a Discord message, and a member perceives
// all three the same way: an upload, a sticker, and the picture a link
// preview unfurls into.

// maxMediaPerMessage bounds one message. Discord permits ten attachments and
// several embeds; forwarding all of them off a single spammed message is a
// cost the agent's own budget should not have to be the only guard against.
const maxMediaPerMessage = 8

func mediaOf(msg discord.Message) []utils.AgentAttachment {
	var out []utils.AgentAttachment

	for _, a := range msg.Attachments {
		if len(out) >= maxMediaPerMessage {
			return out
		}
		att := utils.AgentAttachment{
			URL:      a.URL,
			Filename: a.Filename,
			Size:     a.Size,
		}
		if a.ContentType != nil {
			att.ContentType = *a.ContentType
		}
		// Discord's alt text. Somebody who bothers to write one is saying
		// what the image is for, which is worth more than anything inferred
		// from the picture.
		if a.Description != nil {
			att.Description = *a.Description
		}
		if a.DurationSecs != nil {
			att.DurationSecs = *a.DurationSecs
		}
		if a.Width != nil {
			att.Width = *a.Width
		}
		if a.Height != nil {
			att.Height = *a.Height
		}
		out = append(out, att)
	}

	for _, s := range msg.StickerItems {
		if len(out) >= maxMediaPerMessage {
			return out
		}
		out = append(out, stickerAttachment(s))
	}

	for _, embed := range msg.Embeds {
		if len(out) >= maxMediaPerMessage {
			return out
		}
		// Image first, thumbnail second: a link preview usually has only the
		// thumbnail, an image embed has the full thing, and sending both of
		// the same picture is just paying twice.
		res := embed.Image
		if res == nil {
			res = embed.Thumbnail
		}
		if res == nil || res.URL == "" {
			continue
		}
		out = append(out, utils.AgentAttachment{
			URL:         res.URL,
			ContentType: res.ContentType,
			Filename:    embedName(embed),
			Description: res.Description,
			Width:       res.Width,
			Height:      res.Height,
			Kind:        "embed",
		})
	}

	return out
}

// stickerAttachment builds the CDN URL by hand: disgo's URL() helper hangs off
// discord.Sticker, and a message only carries the lighter MessageSticker.
//
// Lottie stickers are vector animations in JSON, not images -- they are passed
// on with their real content type so the agent describes them by name instead
// of trying and failing to decode one.
func stickerAttachment(s discord.MessageSticker) utils.AgentAttachment {
	ext, contentType := "png", "image/png"
	switch s.FormatType {
	case discord.StickerFormatTypeGIF:
		ext, contentType = "gif", "image/gif"
	case discord.StickerFormatTypeLottie:
		ext, contentType = "json", "application/json"
	}

	return utils.AgentAttachment{
		URL:         fmt.Sprintf("https://cdn.discordapp.com/stickers/%s.%s", s.ID, ext),
		ContentType: contentType,
		Filename:    s.Name + "." + ext,
		Kind:        "sticker",
	}
}

// embedName gives the agent something to call the picture in a link preview,
// preferring the embed's own title and falling back to where it came from.
func embedName(embed discord.Embed) string {
	if title := strings.TrimSpace(embed.Title); title != "" {
		return title
	}
	if embed.Provider != nil && embed.Provider.Name != "" {
		return "image from " + embed.Provider.Name
	}
	return "linked image"
}
