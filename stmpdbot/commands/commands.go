package commands

import (
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
)

// TODO: Organize this also
var Commands = []discord.ApplicationCommandCreate{
	ping,
	avatar,
	eightball,
	lyrics,
	quiz,
	balance,
	withdraw,
	deposit,
	give,
	leaderboard,
	links,
	rank,
	radio,
	//whois,
	version,
	moderation,
	config,
	forgetme,
}

func SetupHandlers(b *stmpdbot.STMPDBot) *handler.Mux {
	rootHandler := handler.New()

	// TODO: This is getting out of hand, find a better place to store and have something like cog baased loading with load unload commands?
	// TODO: Maybe add a help command

	rootHandler.Command("/balance", BalanceHandler(b))
	rootHandler.Command("/withdraw", WithdrawHandler(b))
	rootHandler.Command("/deposit", DepositHandler(b))
	rootHandler.Command("/give", GiveHandler(b))

	rootHandler.Command("/rank", RankHandler(b))
	rootHandler.Command("/leaderboard", LeaderboardHandler(b))

	rootHandler.Command("/links", LinksHandler(b))
	rootHandler.Autocomplete("/links", LinksAutocompleteHandler(b))

	rootHandler.Command("/version", VersionHandler(b))

	rootHandler.Command("/radio", RadioHandler(b))

	rootHandler.Command("/moderation", ModerationHandler(b))
	rootHandler.Component("/modlogs/{userID}/{action}/{page}", ModlogsPaginationHandler(b))
	rootHandler.Component("/leaderboard/{category}/{action}/{page}", LeaderboardPaginationHandler(b))

	rootHandler.Command("/config", ConfigHandler(b))
	rootHandler.Autocomplete("/config", ConfigAutocompleteHandler(b))

	// These were grouped into sub-muxes mounted at "/" ("fun" and "extras"). That
	// stopped working in disgo v0.19 and took /ping, /avatar, /8ball, /lyrics and
	// /quiz down with it -- silently, which is why it went unnoticed for a day.
	//
	// v0.18's splitPath used strings.FieldsFunc, which drops empty fields, so
	// splitPath("/") was []. The pattern loop in Mux.Match had nothing to iterate and
	// fell through to the mounted mux's own routes. v0.19 changed it to TrimPrefix
	// plus Split, so splitPath("/") is [""] -- one empty string. The loop now runs
	// once and compares "" against "ping", does not match, and the mount is skipped.
	//
	// Nothing reports this. Mux.Handle returns nil when no route matches, so the
	// interaction is neither answered nor logged and Discord shows "The application
	// did not respond" with an empty bot log.
	//
	// Registered flat, like every other command above. A sub-mux is only worth having
	// at a real path prefix, and these are all top-level commands.
	rootHandler.Command("/8ball", EightBallHandler)
	rootHandler.Command("/lyrics", LyricsHandler(b))
	rootHandler.Autocomplete("/lyrics", LyricsAutocompleteHandler(b))
	rootHandler.Command("/quiz", QuizHandler(b))
	rootHandler.Command("/avatar", AvatarHandler)
	rootHandler.Command("/ping", PingHandler)
	rootHandler.Command("/forgetme", ForgetMeHandler(b))

	// h.Command("/whois", WhoisHandler)

	return rootHandler
}
