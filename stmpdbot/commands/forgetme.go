package commands

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/utils"
)

// The persona forms memories about people from ordinary conversation, and
// nobody in a public Discord opted into that. This is the way out, and it is a
// real delete rather than a flag: it erases every memory this bot holds about
// the caller.
//
// It is deliberately not admin-gated and takes no target argument -- you can
// only ever erase yourself, so there is nothing here for someone to abuse
// against another member.
// memoryCount reads naturally in a sentence: "erased 1 memory", "erased 14
// memories".
func memoryCount(n int) string {
	if n == 1 {
		return "1 memory"
	}
	return fmt.Sprintf("%d memories", n)
}

var forgetme = discord.SlashCommandCreate{
	Name:        "forgetme",
	Description: "Delete everything the bot remembers about you from past conversations.",
}

func ForgetMeHandler(b *stmpdbot.STMPDBot) handler.CommandHandler {
	return func(e *handler.CommandEvent) error {
		if b.AgentClient == nil {
			return e.CreateMessage(discord.NewMessageCreate().
				WithEmbeds(discord.NewEmbed().
					WithDescription("There's no memory to erase, the AI features are switched off on this bot.").
					WithColor(utils.ColorError)).
				WithEphemeral(true))
		}

		// Ephemeral throughout: what the bot remembers about someone is
		// between it and them, and a public "deleted 14 memories" tells the
		// channel how much it had on them.
		if err := e.DeferCreateMessage(true); err != nil {
			return err
		}

		// Generous: a purge is a list-then-delete loop over however much
		// history the person has, and it must finish rather than half-run.
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		deleted, err := b.AgentClient.Forget(ctx, int64(e.User().ID))
		if err != nil {
			slog.Error("forgetme: failed to erase memories",
				slog.String("user_id", e.User().ID.String()),
				slog.Int("deleted_before_failure", deleted), slog.Any("err", err))

			// Never claim it worked. Say exactly how far it got, because the
			// person is entitled to know what is still held.
			description := "Something went wrong and I couldn't finish erasing your memories. Nothing was deleted."
			if deleted > 0 {
				description = "Something went wrong partway through. I erased " +
					memoryCount(deleted) + ", but there may be more left. Try again."
			}
			_, err = e.UpdateInteractionResponse(discord.NewMessageUpdate().
				WithEmbeds(discord.NewEmbed().
					WithDescription(description).
					WithColor(utils.ColorError)))
			return err
		}

		description := "I didn't have anything remembered about you."
		if deleted > 0 {
			description = "Done. I've erased " + memoryCount(deleted) +
				" about you. I'll still remember things from new conversations, so use this again whenever you want."
		}

		_, err = e.UpdateInteractionResponse(discord.NewMessageUpdate().
			WithEmbeds(discord.NewEmbed().
				WithDescription(description).
				WithColor(utils.ColorSuccess)))
		return err
	}
}
