package ai

import (
	"regexp"
	"strings"
	"unicode"
)

// Deciding what is worth remembering, shared by the two things that mine the
// message history: the one-off backfill in scripts/seed-agent-memory and the
// nightly digest in cmd/agent.
//
// It has to be shared. The two passes feed the same memory store, and a filter
// that drifted between them would mean the bot's sense of a person depended on
// whether it learned about them before or after the backfill ran.
//
// Every message that survives costs an LLM extraction call, so this is a cost
// control before it is anything else. On four years of this server it keeps
// about 2.7% of what it sees -- and that fraction is where the self-disclosure
// actually lives. The rest is "lol", links and commands.

const (
	// MinMessageLength is the shortest message worth an extraction call.
	// Below this a message is a reaction, not a statement: on this corpus
	// 456,343 of 519,388 messages are shorter than it.
	MinMessageLength = 60

	// MinAuthorMessages is how much someone has to have said before the bot
	// forms memories about them at all. Somebody who posted twice in 2023 is
	// not a member of the community in any sense worth remembering, and
	// storing facts about them is cost and exposure for nothing.
	MinAuthorMessages = 20
)

// discordMarkup is stripped before anything is judged sensitive. Custom emoji
// (<:MGDpray:974200733878616104>), mentions (<@424555707187068929>) and CDN
// links are all long runs of digits, and a naive phone-number pattern matches
// every one of them. Before this existed the filter called 9,302 messages
// sensitive; nearly all of them were emoji, and they included most of the
// messages with any personality in them. Afterwards: 7.
var discordMarkup = regexp.MustCompile(`<a?:\w+:\d+>|<@[!&]?\d+>|<#\d+>|https?://\S+`)

// sensitivePattern drops a message outright rather than trusting an extraction
// model to leave it alone. These are things nobody consented to having put in
// a vector database, and the cheapest place to enforce that is before the API
// call rather than after.
//
// Applied to the scrubbed text, and every pattern demands real structure: a
// bare run of digits is a snowflake far more often than a phone number, so the
// phone patterns require separators in phone-like positions.
var sensitivePattern = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`[\w.+-]+@[\w-]+\.[a-z]{2,}`,                                         // email addresses
	`\+\d{1,3}[\s.-]\d{2,4}[\s.-]\d{3,4}[\s.-]?\d{0,4}`,                  // +CC NNN NNN NNNN
	`\b\d{3}[\s.-]\d{3}[\s.-]\d{4}\b`,                                    // NNN-NNN-NNNN
	`discord\.gg/\S+`,                                                    // invites
	`\b(?:sk|xox[bp]|ghp)_[A-Za-z0-9_-]{8,}`,                             // api tokens
	`\b\d{1,5}\s+[A-Za-z]+\s+(?:street|st|road|rd|avenue|ave|lane|ln)\b`, // street addresses
	`\b(?:my|his|her|their)\s+(?:address|postcode|zip|phone number)\b`,
}, "|"))

var (
	bareLink = regexp.MustCompile(`^\s*https?://\S+\s*$`)
	command  = regexp.MustCompile(`^\s*[!/$.]\w+`)
)

// isEmojiOnly reports whether a message is nothing but emoji -- unicode ones,
// or Discord's <:name:id> tokens.
//
// Written as "is there a letter left after the markup comes out" rather than
// as a character-class regex on purpose. The regex version of this
// (^[\s\p{So}\p{Sk}:_<>0-9a-zA-Z]*$) also matched any message with no
// punctuation in it, because letters were in the class -- so a real sentence
// that happened not to use a comma was silently thrown away as a reaction.
func isEmojiOnly(s string) bool {
	for _, r := range discordMarkup.ReplaceAllString(s, "") {
		if unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// Verdict says what happened to a message, so a caller can report why its
// corpus shrank rather than just that it did.
type Verdict int

const (
	Keep Verdict = iota
	TooShort
	Noise
	Sensitive
)

// Judge decides whether one message is worth an extraction call, and returns
// the trimmed content when it is.
func Judge(content string) (string, Verdict) {
	content = strings.TrimSpace(content)

	if len(content) < MinMessageLength {
		return "", TooShort
	}
	if bareLink.MatchString(content) || command.MatchString(content) || isEmojiOnly(content) {
		return "", Noise
	}
	if sensitivePattern.MatchString(discordMarkup.ReplaceAllString(content, " ")) {
		return "", Sensitive
	}
	return content, Keep
}
