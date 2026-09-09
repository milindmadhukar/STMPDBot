package commands

// TODO: Yikes what spagetti code, cleanup

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/handler"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/utils"
)

var quiz = discord.SlashCommandCreate{
	Name:        "quiz",
	Description: "Guess the name of the song from the lyrics!",
	Options: []discord.ApplicationCommandOption{
		discord.ApplicationCommandOptionString{
			Name:        "difficulty",
			Description: "The difficulty of the quiz.",
			Required:    true,
			Choices: []discord.ApplicationCommandOptionChoiceString{
				{
					Name:  "Easy - 50 Coins",
					Value: "easy",
				},
				{
					Name:  "Medium - 100 Coins",
					Value: "medium",
				},
				{
					Name:  "Hard - 150 Coins",
					Value: "hard",
				},
				{
					Name:  "Extreme - 200 Coins",
					Value: "extreme",
				},
			},
		},
	},
}

// TODO: Implement a cooldown
// TODO: Maybe use components and like a dialog box for the quiz, idk?
func QuizHandler(b *stmpdbot.STMPDBot) handler.CommandHandler {
	return func(e *handler.CommandEvent) error {
		difficulty := e.SlashCommandInteractionData().String("difficulty")
		var song db.Song
		var err error
		if difficulty == "easy" {
			song, err = b.Queries.GetRandomSongWithLyricsEasy(e.Ctx)
		} else {
			song, err = b.Queries.GetRandomSongWithLyrics(e.Ctx)
		}
		if err != nil {
			return err
		}

		answers := utils.SongAnswers(song)
		lines := strings.Split(song.Lyrics.String, "\n")
		validLines := filterValidLines(lines, answers, lineCountFor(difficulty))
		if len(validLines) == 0 {
			return fmt.Errorf("no valid lines found in lyrics")
		}

		selectedLines := selectLyricLines(validLines, difficulty)
		lyricsToGuessFrom := strings.Join(selectedLines, "\n")

		lyricsGuessEmbed := discord.NewEmbed().
			WithTitle(fmt.Sprintf("Guess the song title from the lyrics! (%s)", difficulty)).
			WithDescription("Guess the song name within 45 seconds.").
			WithColor(utils.ColorSuccess).
			AddField("Lyrics", lyricsToGuessFrom, false)

		err = e.Respond(
			discord.InteractionResponseTypeCreateMessage,
			discord.NewMessageCreate().
				WithEmbeds(lyricsGuessEmbed),
		)
		if err != nil {
			return err
		}

		go func() {
			filterAuthorMessagesFunc := func(messageEvent *events.MessageCreate) bool {
				return messageEvent.Message.Author.ID == e.Member().User.ID
			}

			answerCheckFunc := func(messageEvent *events.MessageCreate) {
				response := messageEvent.Message.Content
				isClose := utils.GuessMatchesSong(song, response)
				var followUpResponseEmbed discord.Embed
				if isClose {
					// TODO: Maybe define it in constants?
					earningsForDifficulty := map[string]int{
						"easy":    50,
						"medium":  100,
						"hard":    150,
						"extreme": 200,
					}
					earnings := earningsForDifficulty[difficulty]

					err := b.Queries.AddCoins(e.Ctx, db.AddCoinsParams{
						ID:      int64(e.Member().User.ID),
						GuildID: int64(*e.GuildID()),
						InHand:  int64(earnings),
					})

					if err != nil {
						slog.Error("Could not add earnings to user for quiz", slog.Any("err", err))
						return
					}

					followUpResponseEmbed = discord.NewEmbed().
						WithTitle(fmt.Sprintf("%sYour guess is correct and you earned %d coins.", utils.TickEmoji, earnings)).
						WithColor(utils.ColorSuccess).
						AddField("Song Name", fmt.Sprintf("%s - %s", song.Artists, song.Name), false).
						WithThumbnail(song.ThumbnailUrl.String)
				} else {
					followUpResponseEmbed = discord.NewEmbed().
						WithTitle(utils.CrossEmoji+" Your guess is incorrect").
						WithColor(utils.ColorError).
						AddField("Song Name", fmt.Sprintf("%s - %s", song.Artists, song.Name), false).
						WithThumbnail(song.ThumbnailUrl.String)
				}

				followUpMessage := discord.NewMessageCreate().
					WithEmbeds(followUpResponseEmbed)

				followUpMessage = followUpMessage.AddComponents(utils.GetSongButtonRows(song)...)

				_, err := b.Client.Rest.CreateFollowupMessage(e.ApplicationID(), e.Token(),
					followUpMessage,
				)
				if err != nil {
					slog.Error("Error while sending response to quiz answered by user", slog.Any("err", err))
				}
			}

			ctx, cancel := context.WithTimeout(e.Ctx, 45*time.Second)
			defer cancel()

			bot.WaitForEvent(b.Client, ctx, filterAuthorMessagesFunc, answerCheckFunc, func() {
				timerExpiredEmbed := discord.NewEmbed().
					WithTitle(utils.CrossEmoji+" Oops, you ran out of time").
					WithColor(utils.ColorError).
					AddField("Song Name", fmt.Sprintf("%s - %s", song.Artists, song.Name), false).
					WithThumbnail(song.ThumbnailUrl.String)

				_, err := b.Client.Rest.CreateFollowupMessage(e.ApplicationID(), e.Token(),
					discord.NewMessageCreate().
						WithEmbeds(timerExpiredEmbed),
				)

				if err != nil {
					slog.Error("Error while sending timeout response for quiz", slog.Any("err", err))
				}
			})
		}()

		return nil
	}
}

// filterValidLines drops the lyric lines that would give the answer away, weakening
// the filter until enough of them survive.
//
// Hiding lines that contain the stored name is what this used to do, and it can never
// fire on a name like "Breach (Walk Alone)" because no lyric line contains
// parentheses. So the Breach round offered "You'll never walk alone" as the clue for a
// song whose accepted answer is "Walk Alone". Filtering on every accepted form fixes
// that -- but some accepted forms are ordinary words, and hiding every line containing
// the subtitle of "Melt (Tasty)" can leave nothing to quiz on at all.
//
// Hence a ladder rather than a single rule. Each tier hides less than the one above,
// and the first that leaves enough lines wins. A slightly generous clue beats a failed
// interaction.
func filterValidLines(lines []string, answers []string, need int) []string {
	tiers := [][]string{
		answers,            // every form a correct answer may take
		firstN(answers, 2), // the stored name and the base title
		firstN(answers, 1), // the stored name alone
		nil,                // length only
	}

	var last []string
	for _, terms := range tiers {
		last = dropGiveaways(lines, terms)
		if len(last) >= need {
			return last
		}
	}
	return last
}

// dropGiveaways removes lines that are too short to be a clue, and lines naming any
// of the given titles.
//
// The trimming, the LRC tag stripping and the length rule live in
// utils.CleanLyricLine, because the sing-along needs exactly the same notion of what
// a lyric line is; what stays here is the part that is only about quizzing.
func dropGiveaways(lines []string, titles []string) []string {
	var kept []string
	for _, line := range lines {
		content, ok := utils.CleanLyricLine(line)
		if !ok {
			continue
		}
		if namesATitle(content, titles) {
			continue
		}
		kept = append(kept, content)
	}
	return kept
}

// namesATitle reports whether a line gives away any of the titles.
//
// Empty titles are skipped rather than matched. strings.Contains(x, "") is always
// true, which is how an empty song name used to filter out every line in the song and
// leave the quiz with nothing to show.
func namesATitle(line string, titles []string) bool {
	for _, title := range titles {
		if strings.TrimSpace(title) == "" {
			continue
		}
		if utils.TitleAppearsIn(line, title) {
			return true
		}
	}
	return false
}

func firstN(s []string, n int) []string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// linesPerDifficulty is how many lyric lines a round shows. It is also what
// filterValidLines needs, so that "enough lines survived" is a question about this
// round rather than a number the filter has to guess at.
var linesPerDifficulty = map[string]int{
	"easy":    4,
	"medium":  3,
	"hard":    2,
	"extreme": 1,
}

func lineCountFor(difficulty string) int {
	if count := linesPerDifficulty[difficulty]; count > 0 {
		return count
	}
	return 4
}

func selectLyricLines(lines []string, difficulty string) []string {
	if len(lines) == 0 {
		return []string{}
	}

	count := lineCountFor(difficulty)
	if count > len(lines) {
		count = len(lines)
	}

	maxStart := len(lines) - count
	if maxStart < 0 {
		maxStart = 0
	}
	start := rand.IntN(maxStart + 1)

	return lines[start : start+count]
}
