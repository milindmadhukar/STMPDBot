package listeners

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
	"github.com/jackc/pgx/v5"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
)

func MessageDeleteListener(b *stmpdbot.STMPDBot) bot.EventListener {
	return bot.NewListenerFunc(func(e *events.GuildMessageDelete) {
		// Where to log, and which channel to stay out of, in one round trip.
		config, err := b.Queries.GetDeleteLogRouting(context.Background(), int64(e.GuildID))
		if err != nil {
			// If guild config doesn't exist, silently skip logging
			if errors.Is(err, pgx.ErrNoRows) {
				return
			}
			slog.Error("Failed to get delete logs channel", slog.Any("err", err))
			return
		}

		if !config.DeleteLogsChannel.Valid || config.DeleteLogsChannel.Int64 == 0 {
			return
		}

		// The sing-along deletes a wrong guess on every attempt and empties its
		// channel once a day, and disgo fans MESSAGE_DELETE_BULK out into one event
		// per message -- so without this a single daily wipe would post several
		// hundred "Message Deleted" embeds into the moderation log.
		if config.SingAlongChannel.Valid && int64(e.ChannelID) == config.SingAlongChannel.Int64 {
			return
		}

		channelID := snowflake.ID(config.DeleteLogsChannel.Int64)

		// Build embed based on whether message was cached
		embedBuilder := discord.NewEmbed().
			WithTitle("Message Deleted").
			WithColor(0xFF0000). // Red
			AddField("Channel", fmt.Sprintf("<#%d>", e.ChannelID), true).
			AddField("Message ID", e.MessageID.String(), true).
			WithTimestamp(time.Now().UTC())

		// If message was cached, we have full details
		if e.Message.ID != 0 && e.Message.Author.ID != 0 {
			embedBuilder = embedBuilder.
				AddField("Author", fmt.Sprintf("<@%d> (%s)", e.Message.Author.ID, e.Message.Author.Tag()), true)

			// Add content if available
			if e.Message.Content != "" {
				content := e.Message.Content
				if len(content) > 1024 {
					content = content[:1021] + "..."
				}
				embedBuilder = embedBuilder.AddField("Content", content, false)
			}

			// Add attachment info if any
			if len(e.Message.Attachments) > 0 {
				attachmentList := make([]string, 0, len(e.Message.Attachments))
				for _, att := range e.Message.Attachments {
					attachmentList = append(attachmentList, fmt.Sprintf("[%s](%s)", att.Filename, att.URL))
				}
				embedBuilder = embedBuilder.AddField(fmt.Sprintf("Attachments (%d)", len(e.Message.Attachments)),
					strings.Join(attachmentList, "\n"), false)
			}

			// Add embed info if any
			if len(e.Message.Embeds) > 0 {
				embedBuilder = embedBuilder.AddField("Embeds", fmt.Sprintf("%d embed(s)", len(e.Message.Embeds)), true)
			}
		} else {
			// Message was not cached
			embedBuilder = embedBuilder.WithDescription("⚠️ Message details unavailable (not cached)")
		}

		embed := embedBuilder

		_, err = b.Client.Rest.CreateMessage(channelID, discord.NewMessageCreate().
			WithEmbeds(embed),
		)
		if err != nil {
			slog.Error("Failed to send message delete log", slog.Any("err", err))
		}
	})
}

func MessageUpdateListener(b *stmpdbot.STMPDBot) bot.EventListener {
	return bot.NewListenerFunc(func(e *events.GuildMessageUpdate) {
		// Ignore bot messages
		if e.Message.Author.Bot {
			return
		}

		// Get guild configuration for log channel
		config, err := b.Queries.GetEditLogsChannel(context.Background(), int64(e.GuildID))
		if err != nil {
			// If guild config doesn't exist, silently skip logging
			if errors.Is(err, pgx.ErrNoRows) {
				return
			}
			slog.Error("Failed to get edit logs channel", slog.Any("err", err))
			return
		}

		if !config.Valid || config.Int64 == 0 {
			return
		}

		channelID := snowflake.ID(config.Int64)

		// Build embed
		embedBuilder := discord.NewEmbed().
			WithTitle("Message Edited").
			WithColor(0xFFA500). // Orange
			AddField("Channel", fmt.Sprintf("<#%d>", e.ChannelID), true).
			AddField("Message ID", e.MessageID.String(), true).
			AddField("Author", fmt.Sprintf("<@%d> (%s)", e.Message.Author.ID, e.Message.Author.Tag()), true).
			AddField("Jump to Message", fmt.Sprintf("[Click Here](https://discord.com/channels/%d/%d/%d)",
				e.GuildID, e.ChannelID, e.MessageID), true).
			WithTimestamp(time.Now().UTC())

		// If old message was cached, show before/after
		if e.OldMessage.ID != 0 {
			oldContent := e.OldMessage.Content
			if oldContent == "" {
				oldContent = "*No content*"
			}
			if len(oldContent) > 1024 {
				oldContent = oldContent[:1021] + "..."
			}
			embedBuilder = embedBuilder.AddField("Before", oldContent, false)

			newContent := e.Message.Content
			if newContent == "" {
				newContent = "*No content*"
			}
			if len(newContent) > 1024 {
				newContent = newContent[:1021] + "..."
			}
			embedBuilder = embedBuilder.AddField("After", newContent, false)
		} else {
			// Old message was not cached, only show new content
			embedBuilder = embedBuilder.WithDescription("⚠️ Old message content unavailable (not cached)")

			newContent := e.Message.Content
			if newContent == "" {
				newContent = "*No content*"
			}
			if len(newContent) > 1024 {
				newContent = newContent[:1021] + "..."
			}
			embedBuilder = embedBuilder.AddField("New Content", newContent, false)
		}

		embed := embedBuilder

		_, err = b.Client.Rest.CreateMessage(channelID, discord.NewMessageCreate().
			WithEmbeds(embed),
		)
		if err != nil {
			slog.Error("Failed to send message edit log", slog.Any("err", err))
		}
	})
}
