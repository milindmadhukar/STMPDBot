package utils_test

import (
	"testing"

	"github.com/milindmadhukar/STMPDBot/utils"
)

func TestReactionEmoji(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"animated mention", utils.TickEmoji, "tick:810462879374770186"},
		{"static mention", utils.CrossEmoji, "cross:810462920810561556"},
		{"plain custom emoji", "<:spotify:1328049287640125562>", "spotify:1328049287640125562"},
		{"unicode passes through", "✅", "✅"},
		{"garbage passes through", "not an emoji", "not an emoji"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := utils.ReactionEmoji(tc.in); got != tc.want {
				t.Errorf("ReactionEmoji(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
