package utils

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io"
	"strconv"
	"strings"

	"github.com/disgoorg/snowflake/v2"
)

// CutString truncates str to maxLen runes, using a trailing ellipsis as the
// final rune when it has to cut. maxLen below 1 leaves no room for the string
// nor the ellipsis, so it yields "" instead of slicing out of range.
func CutString(str string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(str)
	if len(runes) > maxLen {
		return string(runes[0:maxLen-1]) + "…"
	}
	return string(runes)
}

func ExtractEmojiParts(emojiStr string) (name string, id snowflake.ID, animated bool) {
	trimmed := strings.Trim(emojiStr, "<>")

	parts := strings.Split(trimmed, ":")

	if len(parts) == 3 {
		// Parse, not MustParse: this runs on every song message via
		// createButton, and a malformed emoji string would panic the handler.
		parsed, err := snowflake.Parse(parts[2])
		if err != nil {
			return "", 0, false
		}

		if parts[0] == "a" {
			animated = true
		}

		name = parts[1]
		id = parsed
	}

	return name, id, animated
}

// ReactionEmoji turns an emoji mention into the form the reaction endpoints take.
//
// Discord's reaction routes want "name:id", not the "<a:name:id>" a message body
// uses, and disgo substitutes the value straight into the request path without
// escaping it -- so passing the mention form produces a URL full of angle brackets
// and a 404 rather than a reaction.
//
// A unicode emoji is already in the right form and passes through unchanged, as does
// anything that does not parse, so a caller never ends up with an empty emoji.
func ReactionEmoji(emoji string) string {
	emoji = strings.TrimSpace(emoji)

	name, id, _ := ExtractEmojiParts(emoji)
	if name == "" || id == 0 {
		return emoji
	}

	// The animated flag is deliberately dropped: it belongs to the message form.
	// A reaction identifies the emoji by name and id whether it animates or not.
	return name + ":" + id.String()
}

// Humanize returns a human-readable string of a number.
func Humanize(xp int32) string {
	if xp < 1000 {
		return strconv.Itoa(int(xp))
	}
	xpFloat := float64(xp) / 1000
	return fmt.Sprintf("%.2fK", xpFloat)
}

func ImageToReader(img image.Image) (io.Reader, error) {
	var buf bytes.Buffer

	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}

	reader := bytes.NewReader(buf.Bytes())
	return reader, nil
}
