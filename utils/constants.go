package utils

const (
	ColorError   = 0xFF0000
	ColorDanger  = 0xFF0000
	ColorSuccess = 0x5c5fea
	ColorWarning = 0xFFA500
	ColorInfo    = 0x3498db
	// TickEmoji carries a trailing space; CrossEmoji does not. Existing callers
	// depend on both, so use them rather than pasting the mention in by hand.
	TickEmoji       = "<a:tick:810462879374770186> "
	CrossEmoji      = "<a:cross:810462920810561556>"
	YoutubeEmoji    = "<:youtube:1328049285123674264>"
	SpotifyEmoji    = "<:spotify:1328049287640125562>"
	AppleMusicEmoji = "<:applemusic:1328049289527562300>"

	// Uploaded to the bot's home guild alongside the three above. Marks are from
	// simple-icons (CC0), recoloured to each service's brand hex. TIDAL's official
	// black would vanish against Discord's dark theme, so it uses a neutral tone
	// that stays legible in both themes.
	BeatportEmoji     = "<:beatport:1543896417716666402>"
	DeezerEmoji       = "<:deezer:1543896426600472576>"
	TidalEmoji        = "<:tidal:1543896436553420812>"
	AmazonMusicEmoji  = "<:amazonmusic:1543896446451847170>"
	YoutubeMusicEmoji = "<:youtubemusic:1543896455444561960>"
)

const (
	RANK_PICTURE_WIDTH  = 680
	RankCardWidth       = 1000
	RankCardHeight      = 300
	RankPanelInset      = 20
	RankPanelRadius     = 24
	RankBarX            = 261
	RankBarY            = 194
	RankBarHeight       = 50
	RankAvatarX         = 43
	RankAvatarY         = 63
	RankAvatarDiameter  = 173
	RankUsernameX       = 284
	RankUsernameY       = 145
	RankXpTextX         = 925
	RankXpTextY         = 150
	RankBlockRightX     = RankCardWidth - RankPanelInset - 20
	RankBlockGap        = 30
	RankNumberY         = 50
	RankLabelY          = 93
	RankLabelFontSize   = 22
	RankNumberFontSize  = 50
	RankUsernameMaxSize = 36
)

// The two marks in the form the reaction endpoints take, derived from the mentions
// above so the ids live in exactly one place.
var (
	TickReaction  = ReactionEmoji(TickEmoji)
	CrossReaction = ReactionEmoji(CrossEmoji)
)
