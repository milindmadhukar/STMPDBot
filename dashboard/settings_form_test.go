package dashboard

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/disgoorg/snowflake/v2"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
)

const testGuild = snowflake.ID(690950056202731521)

func fixtureChannels() []BotChannel {
	return []BotChannel{
		{ID: "100", Name: "general", Type: ChannelTypeText},
		{ID: "101", Name: "announcements", Type: ChannelTypeAnnouncement},
		{ID: "200", Name: "Voice", Type: ChannelTypeVoice},
		{ID: "300", Name: "Category", Type: ChannelTypeCategory},
	}
}

func fixtureRoles() []BotRole {
	return []BotRole{
		{ID: "900", Name: "Moderator"},
		{ID: "901", Name: "Bot Integration", Managed: true},
		{ID: testGuild.String(), Name: "@everyone"},
	}
}

func fixtureGuild() db.Guild {
	return db.Guild{
		GuildID:             int64(testGuild),
		ModlogsChannel:      pgtype.Int8{Int64: 100, Valid: true},
		AnniversaryHour:     9,
		AnniversaryTimezone: "Asia/Kolkata",
		XpMultiplier:        1.0,
		LevelUpRole:         pgtype.Int8{Int64: 900, Valid: true},
		LevelUpRoleLevel:    13,

		SingAlongChannel:         pgtype.Int8{Int64: 100, Valid: true},
		SingAlongHour:            9,
		SingAlongTimezone:        "Asia/Kolkata",
		SingAlongCooldownMinutes: 10,
	}
}

func build(t *testing.T, form url.Values) (db.UpdateGuildConfigParams, []string) {
	t.Helper()
	s := &Server{opts: testOptions(t)}
	return s.buildUpdate(fixtureGuild(), form, testGuild, fixtureChannels(), fixtureRoles())
}

func TestBuildUpdateAcceptsValidChannel(t *testing.T) {
	params, problems := build(t, url.Values{"modlogs_channel": {"101"}})
	if len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if !params.ModlogsChannel.Valid || params.ModlogsChannel.Int64 != 101 {
		t.Errorf("modlogs_channel = %+v, want 101", params.ModlogsChannel)
	}
}

// TestBuildUpdateClearsOnEmpty is the reason the query is a full-row UPDATE
// rather than COALESCE: unsetting a log channel is a first-class action.
func TestBuildUpdateClearsOnEmpty(t *testing.T) {
	params, problems := build(t, url.Values{"modlogs_channel": {""}})
	if len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if params.ModlogsChannel.Valid {
		t.Errorf("an empty submission should clear the setting, got %+v", params.ModlogsChannel)
	}
}

// TestBuildUpdateLeavesAbsentFieldsAlone is what makes per-group saving safe:
// saving Notifications must not wipe the Logging channels.
func TestBuildUpdateLeavesAbsentFieldsAlone(t *testing.T) {
	params, problems := build(t, url.Values{"youtube_notifications_channel": {"100"}})
	if len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if !params.ModlogsChannel.Valid || params.ModlogsChannel.Int64 != 100 {
		t.Errorf("a field absent from the form was modified: %+v", params.ModlogsChannel)
	}
}

// TestBuildUpdateRejectsForeignChannel is the check that stops a crafted form
// pointing this guild's log at a channel in a different server.
func TestBuildUpdateRejectsForeignChannel(t *testing.T) {
	params, problems := build(t, url.Values{"modlogs_channel": {"999999999999999999"}})
	if len(problems) == 0 {
		t.Fatal("an ID from another server was accepted")
	}
	if !strings.Contains(problems[0], "in this server") {
		t.Errorf("unhelpful message: %q", problems[0])
	}
	// The stored value must survive a rejected submission.
	if !params.ModlogsChannel.Valid || params.ModlogsChannel.Int64 != 100 {
		t.Errorf("a rejected value overwrote the stored one: %+v", params.ModlogsChannel)
	}
}

func TestBuildUpdateRejectsWrongChannelType(t *testing.T) {
	// A voice channel cannot receive log embeds.
	if _, problems := build(t, url.Values{"modlogs_channel": {"200"}}); len(problems) == 0 {
		t.Error("a voice channel was accepted as a log channel")
	}
	// ...and a text channel is not somewhere the radio can connect.
	if _, problems := build(t, url.Values{"radio_voice_channel": {"100"}}); len(problems) == 0 {
		t.Error("a text channel was accepted as the radio voice channel")
	}
	// A category is neither.
	if _, problems := build(t, url.Values{"modlogs_channel": {"300"}}); len(problems) == 0 {
		t.Error("a category was accepted as a log channel")
	}
}

func TestBuildUpdateRoleRules(t *testing.T) {
	t.Run("valid role", func(t *testing.T) {
		params, problems := build(t, url.Values{"moderator_role": {"900"}})
		if len(problems) > 0 {
			t.Fatalf("unexpected problems: %v", problems)
		}
		if !params.ModeratorRole.Valid || params.ModeratorRole.Int64 != 900 {
			t.Errorf("moderator_role = %+v", params.ModeratorRole)
		}
	})

	t.Run("managed role rejected", func(t *testing.T) {
		// Integration-managed roles cannot be granted to anyone, so setting one
		// as the moderator role would lock everybody out.
		if _, problems := build(t, url.Values{"moderator_role": {"901"}}); len(problems) == 0 {
			t.Error("a managed role was accepted")
		}
	})

	t.Run("everyone rejected", func(t *testing.T) {
		// @everyone carries the guild's own ID; pinging it is never intended.
		if _, problems := build(t, url.Values{"news_role": {testGuild.String()}}); len(problems) == 0 {
			t.Error("@everyone was accepted as a ping role")
		}
	})

	t.Run("unknown role rejected", func(t *testing.T) {
		if _, problems := build(t, url.Values{"moderator_role": {"123"}}); len(problems) == 0 {
			t.Error("a role from another server was accepted")
		}
	})
}

func TestBuildUpdateNumericFields(t *testing.T) {
	cases := []struct {
		name    string
		form    url.Values
		wantErr bool
	}{
		{"hour in range", url.Values{"anniversary_hour": {"23"}}, false},
		{"hour too large", url.Values{"anniversary_hour": {"24"}}, true},
		{"hour negative", url.Values{"anniversary_hour": {"-1"}}, true},
		{"hour not a number", url.Values{"anniversary_hour": {"noon"}}, true},
		{"valid zone", url.Values{"anniversary_timezone": {"Europe/Amsterdam"}}, false},
		{"bogus zone", url.Values{"anniversary_timezone": {"Mars/Olympus"}}, true},
		{"empty zone", url.Values{"anniversary_timezone": {""}}, true},
		{"xp in range", url.Values{"xp_multiplier": {"2.5"}}, false},
		{"xp too large", url.Values{"xp_multiplier": {"9"}}, true},
		{"xp too small", url.Values{"xp_multiplier": {"0"}}, true},
		{"xp not a number", url.Values{"xp_multiplier": {"lots"}}, true},
		{"level in range", url.Values{"level_up_role_level": {"13"}}, false},
		{"level zero grants immediately", url.Values{"level_up_role_level": {"0"}}, false},
		{"level negative", url.Values{"level_up_role_level": {"-1"}}, true},
		{"level too large", url.Values{"level_up_role_level": {"1001"}}, true},
		{"level not a number", url.Values{"level_up_role_level": {"thirteen"}}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := build(t, tc.form)
			if tc.wantErr && len(problems) == 0 {
				t.Errorf("%v was accepted", tc.form)
			}
			if !tc.wantErr && len(problems) > 0 {
				t.Errorf("%v was rejected: %v", tc.form, problems)
			}
		})
	}
}

// TestBuildUpdateRejectsRatherThanClamps: silently storing a different number
// than the one typed is worse than refusing it.
func TestBuildUpdateRejectsRatherThanClamps(t *testing.T) {
	params, problems := build(t, url.Values{"xp_multiplier": {"99"}})
	if len(problems) == 0 {
		t.Fatal("an out-of-range multiplier was accepted")
	}
	if params.XpMultiplier != 1.0 {
		t.Errorf("XpMultiplier = %v; a rejected value must leave the stored one alone", params.XpMultiplier)
	}
}

func TestChangedFields(t *testing.T) {
	before := fixtureGuild()

	params, _ := build(t, url.Values{
		"modlogs_channel":  {"101"},
		"anniversary_hour": {"10"},
	})

	changed := changedFields(before, params)
	if len(changed) != 2 {
		t.Fatalf("changed = %v, want exactly the two fields that moved", changed)
	}
	if changed[0] != "anniversary_hour" || changed[1] != "modlogs_channel" {
		t.Errorf("changed = %v", changed)
	}

	// A no-op save should report nothing changed.
	same, _ := build(t, url.Values{"modlogs_channel": {"100"}})
	if got := changedFields(before, same); len(got) != 0 {
		t.Errorf("a no-op save reported changes: %v", got)
	}
}

func TestOptionsForAlwaysOffersNotSet(t *testing.T) {
	options := optionsFor(kindTextChannel, "", testGuild, fixtureChannels(), fixtureRoles())
	if len(options) == 0 || options[0].ID != "" {
		t.Fatal("the first option must be the empty one, or a setting cannot be cleared")
	}
	if !options[0].Selected {
		t.Error("with nothing set, the empty option should be selected")
	}
}

// TestOptionsForKeepsUnknownSelection: a channel the bot can no longer see must
// stay in the dropdown, or the next save would silently clear it.
func TestOptionsForKeepsUnknownSelection(t *testing.T) {
	options := optionsFor(kindTextChannel, "555", testGuild, fixtureChannels(), fixtureRoles())

	var found bool
	for _, o := range options {
		if o.ID == "555" {
			found = true
			if !o.Selected {
				t.Error("the stored value should stay selected")
			}
			if !strings.Contains(o.Label, "not found") {
				t.Errorf("label should flag it as missing, got %q", o.Label)
			}
		}
	}
	if !found {
		t.Error("a stored-but-unknown ID vanished from the dropdown")
	}
}

func TestOptionsForFiltersByKind(t *testing.T) {
	text := optionsFor(kindTextChannel, "", testGuild, fixtureChannels(), fixtureRoles())
	for _, o := range text {
		if o.ID == "200" || o.ID == "300" {
			t.Errorf("a voice channel or category was offered as a text channel: %q", o.Label)
		}
	}

	voice := optionsFor(kindVoiceChannel, "", testGuild, fixtureChannels(), fixtureRoles())
	for _, o := range voice {
		if o.ID == "100" || o.ID == "101" {
			t.Errorf("a text channel was offered as a voice channel: %q", o.Label)
		}
	}

	roles := optionsFor(kindRole, "", testGuild, fixtureChannels(), fixtureRoles())
	for _, o := range roles {
		if o.ID == "901" {
			t.Error("a managed role was offered")
		}
		if o.ID == testGuild.String() {
			t.Error("@everyone was offered")
		}
	}
}

// UpdateGuildConfig is a full-row update, so a field the form does not submit
// must still be carried forward. Leaving one at its zero value here is a silent
// wipe of whatever was configured -- which is exactly what adding the two
// level-up columns to the query would have caused.
func TestBuildUpdateCarriesLevelUpConfigForward(t *testing.T) {
	params, problems := build(t, url.Values{"modlogs_channel": {"101"}})
	if len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}

	if !params.LevelUpRole.Valid || params.LevelUpRole.Int64 != 900 {
		t.Errorf("LevelUpRole = %+v, want the stored role carried forward", params.LevelUpRole)
	}
	if params.LevelUpRoleLevel != 13 {
		t.Errorf("LevelUpRoleLevel = %d, want 13 carried forward", params.LevelUpRoleLevel)
	}
}

// A rejected level must not overwrite the stored one either.
func TestBuildUpdateRejectedLevelLeavesStoredValue(t *testing.T) {
	params, problems := build(t, url.Values{"level_up_role_level": {"5000"}})
	if len(problems) == 0 {
		t.Fatal("an out-of-range level was accepted")
	}
	if params.LevelUpRoleLevel != 13 {
		t.Errorf("LevelUpRoleLevel = %d; a rejected value must leave the stored one alone", params.LevelUpRoleLevel)
	}
}

// The level-up role goes through the same role rules as every other role field.
func TestBuildUpdateLevelUpRoleRules(t *testing.T) {
	if _, problems := build(t, url.Values{"level_up_role": {"901"}}); len(problems) == 0 {
		t.Error("a managed role was accepted as the level-up role")
	}
	if _, problems := build(t, url.Values{"level_up_role": {testGuild.String()}}); len(problems) == 0 {
		t.Error("@everyone was accepted as the level-up role")
	}
	params, problems := build(t, url.Values{"level_up_role": {""}})
	if len(problems) > 0 {
		t.Fatalf("clearing the level-up role was rejected: %v", problems)
	}
	if params.LevelUpRole.Valid {
		t.Error("an empty submission must clear the level-up role")
	}
}

// The sing-along columns went onto the same full-row UpdateGuildConfig as everything
// else, which means a save of ANY other group carries them. If buildUpdate ever
// stops carrying one forward, saving the Logging panel silently wipes it.
func TestBuildUpdateCarriesSingAlongThroughOtherSaves(t *testing.T) {
	before := fixtureGuild()

	params, problems := build(t, url.Values{"modlogs_channel": {"101"}})
	if len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}

	if params.SingAlongChannel != before.SingAlongChannel {
		t.Errorf("SingAlongChannel = %+v, want it carried forward", params.SingAlongChannel)
	}
	if params.SingAlongHour != before.SingAlongHour {
		t.Errorf("SingAlongHour = %d, want %d", params.SingAlongHour, before.SingAlongHour)
	}
	if params.SingAlongTimezone != before.SingAlongTimezone {
		t.Errorf("SingAlongTimezone = %q, want %q", params.SingAlongTimezone, before.SingAlongTimezone)
	}
	if params.SingAlongCooldownMinutes != before.SingAlongCooldownMinutes {
		t.Errorf("SingAlongCooldownMinutes = %d, want %d",
			params.SingAlongCooldownMinutes, before.SingAlongCooldownMinutes)
	}
}

func TestBuildUpdateSingAlongChannel(t *testing.T) {
	params, problems := build(t, url.Values{"sing_along_channel": {"101"}})
	if len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if !params.SingAlongChannel.Valid || params.SingAlongChannel.Int64 != 101 {
		t.Errorf("sing_along_channel = %+v, want 101", params.SingAlongChannel)
	}

	// Clearing it is how the feature is switched off from the dashboard.
	params, problems = build(t, url.Values{"sing_along_channel": {""}})
	if len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if params.SingAlongChannel.Valid {
		t.Error("an empty sing-along channel must clear the setting")
	}

	// A voice channel is not somewhere a lyric can be posted.
	if _, problems := build(t, url.Values{"sing_along_channel": {"200"}}); len(problems) == 0 {
		t.Error("a voice channel was accepted as the sing-along channel")
	}
}

func TestBuildUpdateSingAlongCooldownBounds(t *testing.T) {
	before := fixtureGuild()

	for _, valid := range []string{"0", "10", "360"} {
		params, problems := build(t, url.Values{singAlongCooldownKey: {valid}})
		if len(problems) > 0 {
			t.Errorf("cooldown %s was rejected: %v", valid, problems)
		}
		if got := strconv.Itoa(int(params.SingAlongCooldownMinutes)); got != valid {
			t.Errorf("cooldown = %s, want %s", got, valid)
		}
	}

	// 360 minutes is Discord's own slowmode ceiling of 21600 seconds; past it the
	// value could not be applied even if it were stored.
	for _, invalid := range []string{"-1", "361", "ten"} {
		params, problems := build(t, url.Values{singAlongCooldownKey: {invalid}})
		if len(problems) == 0 {
			t.Errorf("cooldown %q was accepted", invalid)
		}
		if params.SingAlongCooldownMinutes != before.SingAlongCooldownMinutes {
			t.Errorf("a rejected cooldown must leave the stored one alone, got %d",
				params.SingAlongCooldownMinutes)
		}
	}
}

func TestBuildUpdateSingAlongTimezone(t *testing.T) {
	params, problems := build(t, url.Values{"sing_along_timezone": {"Europe/Amsterdam"}})
	if len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if params.SingAlongTimezone != "Europe/Amsterdam" {
		t.Errorf("SingAlongTimezone = %q", params.SingAlongTimezone)
	}

	// "Local" is what utils.ValidateTimezone exists to refuse: time.LoadLocation
	// accepts it and binds it to whatever zone the log config set as time.Local,
	// which is not a schedule anybody chose.
	for _, invalid := range []string{"", "Local", "Mars/Olympus"} {
		params, problems := build(t, url.Values{"sing_along_timezone": {invalid}})
		if len(problems) == 0 {
			t.Errorf("timezone %q was accepted", invalid)
		}
		if params.SingAlongTimezone != "Asia/Kolkata" {
			t.Errorf("a rejected timezone must leave the stored one alone, got %q",
				params.SingAlongTimezone)
		}
	}
}

func TestChangedFieldsReportsSingAlong(t *testing.T) {
	before := fixtureGuild()

	params, _ := build(t, url.Values{
		"sing_along_channel":  {"101"},
		"sing_along_hour":     {"20"},
		"sing_along_timezone": {"Europe/Amsterdam"},
		singAlongCooldownKey:  {"15"},
	})

	changed := changedFields(before, params)
	want := map[string]bool{
		"sing_along_channel":  true,
		"sing_along_hour":     true,
		"sing_along_timezone": true,
		singAlongCooldownKey:  true,
	}
	if len(changed) != len(want) {
		t.Fatalf("changed = %v, want the four sing-along fields", changed)
	}
	for _, field := range changed {
		if !want[field] {
			t.Errorf("unexpected changed field %q", field)
		}
	}
}
