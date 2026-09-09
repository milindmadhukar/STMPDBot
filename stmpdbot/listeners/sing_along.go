package listeners

import (
	"context"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/stmpdbot/handlers"
	"github.com/milindmadhukar/STMPDBot/utils"
)

// singAlongCoinsPerLine is what one correct lyric line pays.
//
// Small next to the quiz's 50 to 200, because the quiz is a whole song guessed from
// a fragment once, while this is a line at a time all day. Discord's slowmode caps a
// member at one attempt per cooldown, so at the default ten minutes the ceiling is
// about 3600 coins a day for somebody who never gets one wrong and never sleeps.
const singAlongCoinsPerLine = 25

// singAlongMarkHold is how long an attempt wears its mark.
//
// The same for both, so the channel reads consistently: a correct line keeps its
// tick for this long and then loses the reaction, and a wrong one wears its cross
// for this long and is then deleted. Long enough that the person who typed it sees
// which it was, short enough that the channel is not left covered in marks.
const singAlongMarkHold = 5 * time.Second

// SingAlongListener scores the daily sing-along.
//
// The first thing it does is a map lookup on the channel, because it runs on the
// gateway's event goroutine for every message in every channel of every guild and
// almost none of them are sing-along answers. Everything after that lookup -- the
// deletes, the reaction, the coins -- is handed to a goroutine, since rest.WithDelay
// blocks the caller for the whole of its delay.
func SingAlongListener(b *stmpdbot.STMPDBot) bot.EventListener {
	return bot.NewListenerFunc(func(e *events.MessageCreate) {
		if e.Message.Author.Bot || e.Message.Author.System || e.GuildID == nil {
			return
		}

		round, ok := b.SingAlong.Round(e.ChannelID)
		if !ok || round.Complete {
			return
		}

		switch round.Judge(e.Message.Content) {
		case utils.SingAlongEcho:
			// Somebody typing the line that is already on screen. Not an answer, but
			// deleting it would be reading the room badly.
			return

		case utils.SingAlongWrong:
			// Marked before it goes, rather than vanishing without explanation. A
			// message that simply disappears reads as the bot being broken; a cross
			// says it was read and judged, and the five seconds are enough to see it.
			go func() {
				if err := b.Client.Rest.AddReaction(e.ChannelID, e.Message.ID, utils.CrossReaction); err != nil {
					slog.Debug("Could not mark a wrong sing-along answer",
						slog.String("message_id", e.Message.ID.String()),
						slog.Any("err", err))
				}
				// WithDelay holds this goroutine for the wait, which is why none of
				// this runs on the gateway's.
				if err := b.Client.Rest.DeleteMessage(e.ChannelID, e.Message.ID,
					rest.WithDelay(singAlongMarkHold)); err != nil {
					slog.Debug("Could not delete a wrong sing-along answer",
						slog.String("message_id", e.Message.ID.String()),
						slog.Any("err", err))
				}
			}()
			return

		case utils.SingAlongCorrect:
			go awardSingAlongLine(b, e, round)
		}
	})
}

// awardSingAlongLine settles a correct line: decide the winner, pay them, mark the
// message, and close the round out if that was the last line.
func awardSingAlongLine(b *stmpdbot.STMPDBot, e *events.MessageCreate, round utils.SingAlongRound) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Two compare-and-swaps, in memory and then in the database, because two members
	// can answer the same line in the same instant. The database is the one that
	// decides -- it is the only copy two bot instances share -- but the in-memory
	// swap comes first so the common case never reaches Postgres twice.
	advanced, won := b.SingAlong.Advance(e.ChannelID, round.Cursor)
	if !won {
		return
	}

	rows, err := b.Queries.AdvanceSingAlongCursor(ctx, db.AdvanceSingAlongCursorParams{
		GuildID: int64(*e.GuildID),
		Cursor:  int32(round.Cursor),
	})
	if err != nil {
		slog.Error("Failed to advance the sing-along round", slog.Any("err", err))
		return
	}
	if rows == 0 {
		return
	}

	awardSingAlongCoins(ctx, b, int64(e.Message.Author.ID), int64(*e.GuildID))

	if err := b.Client.Rest.AddReaction(e.ChannelID, e.Message.ID, utils.TickReaction); err != nil {
		slog.Debug("Could not mark a correct sing-along answer",
			slog.String("message_id", e.Message.ID.String()),
			slog.Any("err", err))
	} else if err := b.Client.Rest.RemoveOwnReaction(e.ChannelID, e.Message.ID,
		utils.TickReaction, rest.WithDelay(singAlongMarkHold)); err != nil {
		slog.Debug("Could not clear a sing-along tick",
			slog.String("message_id", e.Message.ID.String()),
			slog.Any("err", err))
	}

	if !advanced.Complete {
		return
	}

	if err := b.Queries.CompleteSingAlongRound(ctx, int64(*e.GuildID)); err != nil {
		slog.Error("Failed to close out the sing-along round", slog.Any("err", err))
	}

	if _, err := b.Client.Rest.CreateMessage(e.ChannelID,
		handlers.SingAlongCompletionMessage(advanced)); err != nil {
		slog.Error("Failed to post the sing-along completion", slog.Any("err", err))
	}

	slog.Info("A sing-along round was sung out",
		slog.Int64("guild_id", int64(*e.GuildID)),
		slog.Int("lines", len(advanced.Lines)))
}

// awardSingAlongCoins pays for a line, creating the member's row if this is the
// first thing they have ever earned.
//
// AwardCoins is an UPDATE matched on (id, guild_id), so a member with no row would
// otherwise be passed over in silence -- and the award is the whole reward for
// playing, so losing it quietly is the worst way for this to fail.
func awardSingAlongCoins(ctx context.Context, b *stmpdbot.STMPDBot, userID, guildID int64) {
	award := db.AwardCoinsParams{ID: userID, GuildID: guildID, InHand: singAlongCoinsPerLine}

	rows, err := b.Queries.AwardCoins(ctx, award)
	if err != nil {
		slog.Error("Failed to award sing-along coins", slog.Any("err", err))
		return
	}
	if rows > 0 {
		return
	}

	if _, err := b.Queries.CreateUser(ctx, db.CreateUserParams{ID: userID, GuildID: guildID}); err != nil {
		slog.Error("Failed to create a user for a sing-along award", slog.Any("err", err))
		return
	}
	if _, err := b.Queries.AwardCoins(ctx, award); err != nil {
		slog.Error("Failed to award sing-along coins", slog.Any("err", err))
	}
}
