package dashboard

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/utils"
)

// buildUpdate turns a submitted form into UpdateGuildConfigParams, validating
// every field against the guild's real channels and roles first.
//
// The check that matters most is that a submitted ID belongs to THIS guild.
// Without it, a crafted form could point one server's moderation log at a
// channel in another server the bot happens to be in.
//
// A field absent from the form keeps its current value; a field present but
// empty is cleared. That is what lets the page save one group at a time while
// still being able to unset a setting.
func (s *Server) buildUpdate(
	guild db.Guild,
	form url.Values,
	guildID snowflake.ID,
	channels []BotChannel,
	roles []BotRole,
) (db.UpdateGuildConfigParams, []string) {
	var problems []string

	textChannels := map[string]BotChannel{}
	voiceChannels := map[string]BotChannel{}
	for _, c := range channels {
		switch {
		case c.IsText():
			textChannels[c.ID] = c
		case c.Type == ChannelTypeVoice:
			voiceChannels[c.ID] = c
		}
	}
	roleByID := RoleLookup(roles)

	// channelField resolves one channel setting, defaulting to the stored value
	// when the field was not part of the submitted group.
	channelField := func(key, label string, current pgtype.Int8, allowed map[string]BotChannel, kindName string) pgtype.Int8 {
		raw, present := form[key]
		if !present {
			return current
		}
		value := strings.TrimSpace(raw[0])
		if value == "" {
			return pgtype.Int8{}
		}
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: not a valid ID.", label))
			return current
		}
		if _, ok := allowed[value]; !ok {
			problems = append(problems, fmt.Sprintf("%s: must be a %s channel in this server.", label, kindName))
			return current
		}
		return pgtype.Int8{Int64: id, Valid: true}
	}

	roleField := func(key, label string, current pgtype.Int8) pgtype.Int8 {
		raw, present := form[key]
		if !present {
			return current
		}
		value := strings.TrimSpace(raw[0])
		if value == "" {
			return pgtype.Int8{}
		}
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: not a valid ID.", label))
			return current
		}
		role, ok := roleByID[value]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: must be a role in this server.", label))
			return current
		}
		// @everyone carries the guild's own ID; pinging it is never what an
		// admin means to configure here.
		if value == guildID.String() {
			problems = append(problems, fmt.Sprintf("%s: @everyone cannot be used.", label))
			return current
		}
		if role.Managed {
			problems = append(problems, fmt.Sprintf("%s: %q is managed by an integration and cannot be assigned.", label, role.Name))
			return current
		}
		return pgtype.Int8{Int64: id, Valid: true}
	}

	params := db.UpdateGuildConfigParams{
		GuildID: int64(guildID),

		ModlogsChannel:       channelField("modlogs_channel", "Moderation log", guild.ModlogsChannel, textChannels, "text"),
		LeaveJoinLogsChannel: channelField("leave_join_logs_channel", "Joins & leaves", guild.LeaveJoinLogsChannel, textChannels, "text"),
		DeleteLogsChannel:    channelField("delete_logs_channel", "Deleted messages", guild.DeleteLogsChannel, textChannels, "text"),
		EditLogsChannel:      channelField("edit_logs_channel", "Edited messages", guild.EditLogsChannel, textChannels, "text"),
		VoiceLogsChannel:     channelField("voice_logs_channel", "Voice activity", guild.VoiceLogsChannel, textChannels, "text"),
		MemberLogsChannel:    channelField("member_logs_channel", "Profile changes", guild.MemberLogsChannel, textChannels, "text"),
		WelcomesChannel:      channelField("welcomes_channel", "Welcomes", guild.WelcomesChannel, textChannels, "text"),
		BotChannel:           channelField("bot_channel", "Bot channel", guild.BotChannel, textChannels, "text"),

		YoutubeNotificationsChannel:     channelField("youtube_notifications_channel", "YouTube channel", guild.YoutubeNotificationsChannel, textChannels, "text"),
		RedditNotificationsChannel:      channelField("reddit_notifications_channel", "Reddit channel", guild.RedditNotificationsChannel, textChannels, "text"),
		StmpdNotificationsChannel:       channelField("stmpd_notifications_channel", "STMPD channel", guild.StmpdNotificationsChannel, textChannels, "text"),
		TourNotificationsChannel:        channelField("tour_notifications_channel", "Tour channel", guild.TourNotificationsChannel, textChannels, "text"),
		AnniversaryNotificationsChannel: channelField("anniversary_notifications_channel", "Anniversaries channel", guild.AnniversaryNotificationsChannel, textChannels, "text"),

		RadioVoiceChannel: channelField("radio_voice_channel", "Radio voice channel", guild.RadioVoiceChannel, voiceChannels, "voice"),

		YoutubeNotificationsRole:     roleField("youtube_notifications_role", "YouTube role", guild.YoutubeNotificationsRole),
		RedditNotificationsRole:      roleField("reddit_notifications_role", "Reddit role", guild.RedditNotificationsRole),
		StmpdNotificationsRole:       roleField("stmpd_notifications_role", "STMPD role", guild.StmpdNotificationsRole),
		TourNotificationsRole:        roleField("tour_notifications_role", "Tour role", guild.TourNotificationsRole),
		AnniversaryNotificationsRole: roleField("anniversary_notifications_role", "Anniversaries role", guild.AnniversaryNotificationsRole),
		ModeratorRole:                roleField("moderator_role", "Moderator role", guild.ModeratorRole),
		NewsRole:                     roleField("news_role", "News role", guild.NewsRole),
		LevelUpRole:                  roleField("level_up_role", "Level-up role", guild.LevelUpRole),

		SingAlongChannel: channelField("sing_along_channel", "Sing-along channel", guild.SingAlongChannel, textChannels, "text"),

		AnniversaryHour:     guild.AnniversaryHour,
		AnniversaryTimezone: guild.AnniversaryTimezone,
		// Carried forward for the same reason as LevelUpRoleLevel below.
		SingAlongHour:            guild.SingAlongHour,
		SingAlongTimezone:        guild.SingAlongTimezone,
		SingAlongCooldownMinutes: guild.SingAlongCooldownMinutes,
		XpMultiplier:             guild.XpMultiplier,
		// Carried forward unless the form says otherwise. UpdateGuildConfig is a
		// full-row update, so a field left at its zero value here is a silent wipe.
		LevelUpRoleLevel: guild.LevelUpRoleLevel,
	}

	if raw, ok := form["anniversary_hour"]; ok {
		hour, err := strconv.Atoi(strings.TrimSpace(raw[0]))
		if err != nil || hour < 0 || hour > 23 {
			problems = append(problems, "Anniversary hour: must be between 0 and 23.")
		} else {
			params.AnniversaryHour = int32(hour)
		}
	}

	if raw, ok := form["anniversary_timezone"]; ok {
		tz := strings.TrimSpace(raw[0])
		if tz == "" {
			problems = append(problems, "Anniversary timezone: cannot be empty.")
		} else if _, err := time.LoadLocation(tz); err != nil {
			problems = append(problems, fmt.Sprintf("Anniversary timezone: %q is not a valid IANA zone.", tz))
		} else {
			params.AnniversaryTimezone = tz
		}
	}

	if raw, ok := form["sing_along_hour"]; ok {
		hour, err := strconv.Atoi(strings.TrimSpace(raw[0]))
		if err != nil || hour < 0 || hour > 23 {
			problems = append(problems, "Sing-along hour: must be between 0 and 23.")
		} else {
			params.SingAlongHour = int32(hour)
		}
	}

	if raw, ok := form["sing_along_timezone"]; ok {
		tz := strings.TrimSpace(raw[0])
		// ValidateTimezone rather than time.LoadLocation, which accepts "Local" and
		// binds it to whatever the log config's zone happens to be -- see
		// utils/timezones.go. A scheduler that resolves the zone the same way the
		// form validated it is the whole point.
		if _, ok := utils.ValidateTimezone(tz); !ok {
			problems = append(problems, fmt.Sprintf("Sing-along timezone: %q is not a valid IANA zone.", tz))
		} else {
			params.SingAlongTimezone = tz
		}
	}

	if raw, ok := form[singAlongCooldownKey]; ok {
		minutes, err := strconv.Atoi(strings.TrimSpace(raw[0]))
		switch {
		case err != nil:
			problems = append(problems, "Sing-along cooldown: must be a whole number of minutes.")
		case minutes < minSingAlongCooldown || minutes > maxSingAlongCooldown:
			// Discord's own slowmode ceiling is 21600 seconds, so anything above six
			// hours could not be applied even if it were stored.
			problems = append(problems, fmt.Sprintf(
				"Sing-along cooldown: must be between %d and %d minutes.",
				minSingAlongCooldown, maxSingAlongCooldown))
		default:
			params.SingAlongCooldownMinutes = int32(minutes)
		}
	}

	if raw, ok := form[levelUpLevelKey]; ok {
		level, err := strconv.Atoi(strings.TrimSpace(raw[0]))
		switch {
		case err != nil:
			problems = append(problems, "Level-up level: must be a whole number.")
		case level < minLevelUpLevel || level > maxLevelUpLevel:
			problems = append(problems, fmt.Sprintf(
				"Level-up level: must be between %d and %d.", minLevelUpLevel, maxLevelUpLevel))
		default:
			params.LevelUpRoleLevel = int32(level)
		}
	}

	if raw, ok := form[xpMultiplierKey]; ok {
		value, err := strconv.ParseFloat(strings.TrimSpace(raw[0]), 64)
		switch {
		case err != nil:
			problems = append(problems, "XP multiplier: must be a number.")
		case value < minXPMultiplier || value > maxXPMultiplier:
			// Rejected rather than clamped: silently storing something other
			// than what was typed is worse than saying no.
			problems = append(problems, fmt.Sprintf(
				"XP multiplier: must be between %.1f and %.1f.", minXPMultiplier, maxXPMultiplier))
		default:
			params.XpMultiplier = value
		}
	}

	return params, problems
}

// changedFields reports which settings a save actually altered, for the audit
// log line and the "saved" confirmation.
func changedFields(before db.Guild, after db.UpdateGuildConfigParams) []string {
	var changed []string

	compare := func(name string, a, b pgtype.Int8) {
		if a.Valid != b.Valid || (a.Valid && a.Int64 != b.Int64) {
			changed = append(changed, name)
		}
	}

	compare("modlogs_channel", before.ModlogsChannel, after.ModlogsChannel)
	compare("leave_join_logs_channel", before.LeaveJoinLogsChannel, after.LeaveJoinLogsChannel)
	compare("delete_logs_channel", before.DeleteLogsChannel, after.DeleteLogsChannel)
	compare("edit_logs_channel", before.EditLogsChannel, after.EditLogsChannel)
	compare("voice_logs_channel", before.VoiceLogsChannel, after.VoiceLogsChannel)
	compare("member_logs_channel", before.MemberLogsChannel, after.MemberLogsChannel)
	compare("welcomes_channel", before.WelcomesChannel, after.WelcomesChannel)
	compare("bot_channel", before.BotChannel, after.BotChannel)
	compare("youtube_notifications_channel", before.YoutubeNotificationsChannel, after.YoutubeNotificationsChannel)
	compare("youtube_notifications_role", before.YoutubeNotificationsRole, after.YoutubeNotificationsRole)
	compare("reddit_notifications_channel", before.RedditNotificationsChannel, after.RedditNotificationsChannel)
	compare("reddit_notifications_role", before.RedditNotificationsRole, after.RedditNotificationsRole)
	compare("stmpd_notifications_channel", before.StmpdNotificationsChannel, after.StmpdNotificationsChannel)
	compare("stmpd_notifications_role", before.StmpdNotificationsRole, after.StmpdNotificationsRole)
	compare("tour_notifications_channel", before.TourNotificationsChannel, after.TourNotificationsChannel)
	compare("tour_notifications_role", before.TourNotificationsRole, after.TourNotificationsRole)
	compare("anniversary_notifications_channel", before.AnniversaryNotificationsChannel, after.AnniversaryNotificationsChannel)
	compare("anniversary_notifications_role", before.AnniversaryNotificationsRole, after.AnniversaryNotificationsRole)
	compare("moderator_role", before.ModeratorRole, after.ModeratorRole)
	compare("news_role", before.NewsRole, after.NewsRole)
	compare("radio_voice_channel", before.RadioVoiceChannel, after.RadioVoiceChannel)
	compare("level_up_role", before.LevelUpRole, after.LevelUpRole)

	if before.AnniversaryHour != after.AnniversaryHour {
		changed = append(changed, "anniversary_hour")
	}
	if before.AnniversaryTimezone != after.AnniversaryTimezone {
		changed = append(changed, "anniversary_timezone")
	}
	compare("sing_along_channel", before.SingAlongChannel, after.SingAlongChannel)

	if before.SingAlongHour != after.SingAlongHour {
		changed = append(changed, "sing_along_hour")
	}
	if before.SingAlongTimezone != after.SingAlongTimezone {
		changed = append(changed, "sing_along_timezone")
	}
	if before.SingAlongCooldownMinutes != after.SingAlongCooldownMinutes {
		changed = append(changed, singAlongCooldownKey)
	}
	if before.XpMultiplier != after.XpMultiplier {
		changed = append(changed, xpMultiplierKey)
	}
	if before.LevelUpRoleLevel != after.LevelUpRoleLevel {
		changed = append(changed, levelUpLevelKey)
	}

	sort.Strings(changed)
	return changed
}
