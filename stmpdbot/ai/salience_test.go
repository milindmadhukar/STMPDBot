package ai

import "testing"

func TestJudge(t *testing.T) {
	t.Parallel()

	long := "i think theres a higher chance martin does a concert without dj'ing, he just sings and thats fine by me"

	for _, tc := range []struct {
		name    string
		content string
		want    Verdict
	}{
		{"a real opinion is kept", long, Keep},
		{"a reaction is too short", "lol same", TooShort},
		{"a bare link is noise", "https://open.spotify.com/track/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Noise},
		{"a command is noise", "/lyrics animals by martin garrix please and thank you very much indeed", Noise},
		{"an email is sensitive", "hit me up at someone.person@example.com if you want the stems for this one", Sensitive},
		{"a phone number is sensitive", "my number is +44 7700 900123 if anyone wants to talk about the record", Sensitive},
		{"an invite is sensitive", "come join the other server we made for this discord.gg/abcdefg it is much better", Sensitive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, got := Judge(tc.content); got != tc.want {
				t.Errorf("Judge(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// The regression that mattered: Discord markup is long runs of digits, and a
// naive phone pattern called 9,302 messages sensitive -- nearly all of them
// emoji, and they were the messages with the most personality in them.
func TestJudgeDoesNotMistakeDiscordMarkupForPersonalData(t *testing.T) {
	t.Parallel()

	for _, content := range []string{
		"LETSGOOOOO, Garrix show in the netherlands <:MGDpray:974200733878616104> so hyped for this one honestly",
		"<@424555707187068929> will there be any watch party sort of arrangement for the ULTRA live stream tonight",
		"What are the chances he does a b2b with Alesso there? <a:dedeath:672490176282361866> would be unreal honestly",
		"<@&1245656819578175539> quick update: inversions set is cancelled, the rest will play according to schedule",
		"maybe you can make wav [.](https://cdn.discordapp.com/emojis/875806693576019988.webp?size=48&name=peep)",
	} {
		if kept, got := Judge(content); got != Keep {
			t.Errorf("Judge(%.60q...) = %v, want Keep (kept=%q)", content, got, kept)
		}
	}
}

// A message that is genuinely sensitive must still be caught even when it is
// wrapped in the markup that used to cause false positives.
func TestJudgeStillCatchesSensitiveTextAmongMarkup(t *testing.T) {
	t.Parallel()

	content := "<@424555707187068929> sure, email me at real.person@example.org <:MGDpray:974200733878616104>"
	if _, got := Judge(content); got != Sensitive {
		t.Errorf("Judge() = %v, want Sensitive -- scrubbing markup must not scrub the email too", got)
	}
}

func TestJudgeTrimsWhatItKeeps(t *testing.T) {
	t.Parallel()

	padded := "   " + "i have been producing for about three years now and still cannot finish a track properly" + "   "
	kept, verdict := Judge(padded)
	if verdict != Keep {
		t.Fatalf("verdict = %v, want Keep", verdict)
	}
	if kept != "i have been producing for about three years now and still cannot finish a track properly" {
		t.Errorf("kept = %q, want it trimmed", kept)
	}
}
