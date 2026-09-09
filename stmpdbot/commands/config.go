package commands

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/snowflake/v2"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/stmpdbot/handlers"
	"github.com/milindmadhukar/STMPDBot/utils"
)

var config = discord.SlashCommandCreate{
	Name:        "config",
	Description: "Configure bot settings for this server",
	Options: []discord.ApplicationCommandOption{
		discord.ApplicationCommandOptionSubCommand{
			Name:        "set-moderator-role",
			Description: "Set the moderator role for this server",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionRole{
					Name:        "role",
					Description: "The role that should have moderator permissions",
					Required:    true,
				},
			},
		},
		discord.ApplicationCommandOptionSubCommand{
			Name:        "set-anniversary-channel",
			Description: "Where to post daily song anniversaries",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionChannel{
					Name:        "channel",
					Description: "The channel to post anniversaries in",
					Required:    true,
					ChannelTypes: []discord.ChannelType{
						discord.ChannelTypeGuildText,
						discord.ChannelTypeGuildNews,
					},
				},
				discord.ApplicationCommandOptionRole{
					Name:        "role",
					Description: "Role to ping (omit to clear the ping)",
					Required:    false,
				},
			},
		},
		discord.ApplicationCommandOptionSubCommand{
			Name:        "set-anniversary-time",
			Description: "What time of day anniversaries post, in your server's timezone",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionInt{
					Name:        "hour",
					Description: "Hour of the day, 0-23, in the timezone below",
					Required:    true,
					MinValue:    &anniversaryMinHour,
					MaxValue:    &anniversaryMaxHour,
				},
				discord.ApplicationCommandOptionString{
					Name:         "timezone",
					Description:  "IANA timezone, e.g. Europe/Amsterdam",
					Required:     true,
					Autocomplete: true,
				},
			},
		},
		discord.ApplicationCommandOptionSubCommand{
			Name:        "anniversary-preview",
			Description: "Privately preview today's anniversary post without sending it",
		},
		discord.ApplicationCommandOptionSubCommand{
			Name:        "disable-anniversaries",
			Description: "Stop posting daily song anniversaries",
		},
		discord.ApplicationCommandOptionSubCommand{
			Name:        "set-sing-along-channel",
			Description: "Where to run the daily sing-along",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionChannel{
					Name:        "channel",
					Description: "Everything in this channel is deleted when each new song drops",
					Required:    true,
					ChannelTypes: []discord.ChannelType{
						discord.ChannelTypeGuildText,
					},
				},
			},
		},
		discord.ApplicationCommandOptionSubCommand{
			Name:        "set-sing-along-time",
			Description: "When the daily lyric drops, and how long members wait between attempts",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionInt{
					Name:        "hour",
					Description: "Hour of the day, 0-23, in the timezone below",
					Required:    true,
					MinValue:    &anniversaryMinHour,
					MaxValue:    &anniversaryMaxHour,
				},
				discord.ApplicationCommandOptionString{
					Name:         "timezone",
					Description:  "IANA timezone, e.g. Europe/Amsterdam",
					Required:     true,
					Autocomplete: true,
				},
				discord.ApplicationCommandOptionInt{
					Name:        "cooldown",
					Description: "Minutes a member waits between attempts, applied as channel slowmode",
					Required:    false,
					MinValue:    &singAlongMinCooldown,
					MaxValue:    &singAlongMaxCooldown,
				},
			},
		},
		discord.ApplicationCommandOptionSubCommand{
			Name:        "sing-along-reroll",
			Description: "Wipe the sing-along channel and start a new song right now",
		},
		discord.ApplicationCommandOptionSubCommand{
			Name:        "disable-sing-along",
			Description: "Stop the daily sing-along",
		},
		discord.ApplicationCommandOptionSubCommand{
			Name:        "view",
			Description: "View current server configuration",
		},
	},
}

// Discord's Min/MaxValue are *int, so the bounds need addressable homes.
var (
	anniversaryMinHour = 0
	anniversaryMaxHour = 23

	// Discord's own slowmode ceiling is 21600 seconds, so six hours is as long a
	// cooldown as could ever actually be applied.
	singAlongMinCooldown = 0
	singAlongMaxCooldown = 360
)

// singAlongDefaultCooldown matches the column default in migration 000030, for the
// case where the guild row cannot be read while setting the schedule.
const singAlongDefaultCooldown = 10

func ConfigHandler(b *stmpdbot.STMPDBot) handler.CommandHandler {
	return func(e *handler.CommandEvent) error {
		// Check if the user has Administrator permission
		if !e.Member().Permissions.Has(discord.PermissionAdministrator) {
			return e.Respond(discord.InteractionResponseTypeCreateMessage,
				discord.NewMessageCreate().
					WithEmbeds(utils.FailureEmbed("Permission Denied",
						"Only administrators can configure bot settings.")).
					WithEphemeral(true),
			)
		}

		data := e.SlashCommandInteractionData()
		subcommand := data.SubCommandName

		switch *subcommand {
		case "set-moderator-role":
			return handleSetModeratorRole(b, e)
		case "set-anniversary-channel":
			return handleSetAnniversaryChannel(b, e)
		case "set-anniversary-time":
			return handleSetAnniversaryTime(b, e)
		case "anniversary-preview":
			return handleAnniversaryPreview(b, e)
		case "disable-anniversaries":
			return handleDisableAnniversaries(b, e)
		case "set-sing-along-channel":
			return handleSetSingAlongChannel(b, e)
		case "set-sing-along-time":
			return handleSetSingAlongTime(b, e)
		case "sing-along-reroll":
			return handleSingAlongReroll(b, e)
		case "disable-sing-along":
			return handleDisableSingAlong(b, e)
		case "view":
			return handleViewConfig(b, e)
		default:
			return e.Respond(discord.InteractionResponseTypeCreateMessage,
				discord.NewMessageCreate().
					WithEmbeds(utils.FailureEmbed("Invalid Command", "Unknown subcommand")).
					WithEphemeral(true),
			)
		}
	}
}

func handleSetModeratorRole(b *stmpdbot.STMPDBot, e *handler.CommandEvent) error {
	data := e.SlashCommandInteractionData()
	role := data.Role("role")
	guildID := *e.GuildID()

	// Update the moderator role in the database
	err := b.Queries.SetModeratorRole(e.Ctx, db.SetModeratorRoleParams{
		GuildID: int64(guildID),
		ModeratorRole: pgtype.Int8{
			Int64: int64(role.ID),
			Valid: true,
		},
	})

	if err != nil {
		return e.Respond(discord.InteractionResponseTypeCreateMessage,
			discord.NewMessageCreate().
				WithEmbeds(utils.FailureEmbed("Configuration Failed",
					fmt.Sprintf("Failed to update moderator role: %s", err.Error()))).
				WithEphemeral(true),
		)
	}

	embed := discord.NewEmbed().
		WithTitle("Moderator Role Updated").
		WithDescription(fmt.Sprintf("The moderator role has been set to <@&%d>", role.ID)).
		AddField("What this means",
			"Members with this role (or Administrator permission) can now use moderation commands like kick, ban, mute, etc.",
			false).
		WithColor(utils.ColorSuccess)

	return e.Respond(discord.InteractionResponseTypeCreateMessage,
		discord.NewMessageCreate().
			WithEmbeds(embed),
	)
}

// ephemeralFailure is the reply shape every misconfiguration in this file uses.
func ephemeralFailure(e *handler.CommandEvent, title, description string) error {
	return e.Respond(discord.InteractionResponseTypeCreateMessage,
		discord.NewMessageCreate().
			WithEmbeds(utils.FailureEmbed(title, description)).
			WithEphemeral(true),
	)
}

// nextAnniversaryPost is when the configured schedule will next fire.
//
// Shown back to the admin because an hour and a timezone are easy to get subtly
// wrong, and "next post in 3 hours" is a far better check than re-reading the two
// values you just typed.
func nextAnniversaryPost(hour int, loc *time.Location) time.Time {
	now := time.Now().In(loc)
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, loc)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func handleSetAnniversaryChannel(b *stmpdbot.STMPDBot, e *handler.CommandEvent) error {
	data := e.SlashCommandInteractionData()
	guildID := *e.GuildID()

	channel := data.Channel("channel")

	// The role is written on every call, so omitting it clears a stale ping rather
	// than silently keeping one the admin thinks they removed.
	role := pgtype.Int8{}
	if r, ok := data.OptRole("role"); ok {
		role = pgtype.Int8{Int64: int64(r.ID), Valid: true}
	}

	if err := b.Queries.SetAnniversaryChannel(e.Ctx, db.SetAnniversaryChannelParams{
		GuildID:                         int64(guildID),
		AnniversaryNotificationsChannel: pgtype.Int8{Int64: int64(channel.ID), Valid: true},
	}); err != nil {
		return ephemeralFailure(e, "Configuration Failed",
			fmt.Sprintf("Failed to set the anniversary channel: %s", err.Error()))
	}

	if err := b.Queries.SetAnniversaryRole(e.Ctx, db.SetAnniversaryRoleParams{
		GuildID:                      int64(guildID),
		AnniversaryNotificationsRole: role,
	}); err != nil {
		return ephemeralFailure(e, "Configuration Failed",
			fmt.Sprintf("Failed to set the anniversary role: %s", err.Error()))
	}

	ping := "no role will be pinged"
	if role.Valid {
		ping = fmt.Sprintf("<@&%d> will be pinged", role.Int64)
	}

	embed := discord.NewEmbed().
		WithTitle("Anniversary Channel Updated").
		WithDescription(fmt.Sprintf("Daily song anniversaries will post in <#%d>, and %s.",
			channel.ID, ping)).
		WithColor(utils.ColorSuccess)

	// Read the schedule back so the admin sees the default rather than assuming one.
	if config, err := b.Queries.GetGuild(e.Ctx, int64(guildID)); err == nil {
		embed = embed.AddField("Schedule",
			fmt.Sprintf("%02d:00 in `%s` — change it with `/config set-anniversary-time`",
				config.AnniversaryHour, config.AnniversaryTimezone), false)
	}

	return e.Respond(discord.InteractionResponseTypeCreateMessage,
		discord.NewMessageCreate().WithEmbeds(embed),
	)
}

func handleSetAnniversaryTime(b *stmpdbot.STMPDBot, e *handler.CommandEvent) error {
	data := e.SlashCommandInteractionData()
	guildID := *e.GuildID()

	hour := data.Int("hour")
	timezone := data.String("timezone")

	// Validate before writing. A zone the scheduler cannot resolve would leave the
	// feature silently dead, with nothing in the channel to show it.
	loc, ok := utils.ValidateTimezone(timezone)
	if !ok {
		return ephemeralFailure(e, "Unknown Timezone",
			fmt.Sprintf("`%s` is not an IANA timezone name. Try `Europe/Amsterdam`, "+
				"`America/New_York`, `Asia/Kolkata` or `UTC`.", timezone))
	}

	if err := b.Queries.SetAnniversarySchedule(e.Ctx, db.SetAnniversaryScheduleParams{
		GuildID:             int64(guildID),
		AnniversaryHour:     int32(hour),
		AnniversaryTimezone: timezone,
	}); err != nil {
		return ephemeralFailure(e, "Configuration Failed",
			fmt.Sprintf("Failed to update the anniversary schedule: %s", err.Error()))
	}

	next := nextAnniversaryPost(hour, loc)

	embed := discord.NewEmbed().
		WithTitle("Anniversary Schedule Updated").
		WithDescription(fmt.Sprintf("Anniversaries will post at **%02d:00** in `%s`.", hour, timezone)).
		AddField("Next post", fmt.Sprintf("<t:%d:F> (<t:%d:R>)", next.Unix(), next.Unix()), false).
		WithColor(utils.ColorSuccess)

	return e.Respond(discord.InteractionResponseTypeCreateMessage,
		discord.NewMessageCreate().WithEmbeds(embed),
	)
}

func handleAnniversaryPreview(b *stmpdbot.STMPDBot, e *handler.CommandEvent) error {
	guildID := *e.GuildID()

	config, err := b.Queries.GetGuild(e.Ctx, int64(guildID))
	if err != nil {
		return ephemeralFailure(e, "Error", "Failed to fetch server configuration")
	}

	content, embeds, err := handlers.PreviewAnniversaries(e.Ctx, b, config.AnniversaryTimezone)
	if err != nil {
		return ephemeralFailure(e, "Preview Failed", err.Error())
	}

	builder := discord.NewMessageCreate().
		WithContent(content).
		WithEphemeral(true)

	if len(embeds) > 0 {
		builder = builder.WithEmbeds(embeds...)
	}

	return e.Respond(discord.InteractionResponseTypeCreateMessage, builder)
}

func handleDisableAnniversaries(b *stmpdbot.STMPDBot, e *handler.CommandEvent) error {
	guildID := *e.GuildID()

	// Only the channel is cleared. The hour and timezone stay, so re-enabling later
	// remembers the schedule the admin already picked.
	if err := b.Queries.SetAnniversaryChannel(e.Ctx, db.SetAnniversaryChannelParams{
		GuildID:                         int64(guildID),
		AnniversaryNotificationsChannel: pgtype.Int8{},
	}); err != nil {
		return ephemeralFailure(e, "Configuration Failed",
			fmt.Sprintf("Failed to disable anniversaries: %s", err.Error()))
	}

	embed := discord.NewEmbed().
		WithTitle("Anniversaries Disabled").
		WithDescription("Daily song anniversaries will no longer be posted. " +
			"Your posting time and timezone have been kept for when you turn it back on.").
		WithColor(utils.ColorSuccess)

	return e.Respond(discord.InteractionResponseTypeCreateMessage,
		discord.NewMessageCreate().WithEmbeds(embed),
	)
}

func handleSetSingAlongChannel(b *stmpdbot.STMPDBot, e *handler.CommandEvent) error {
	guildID := *e.GuildID()
	channel := e.SlashCommandInteractionData().Channel("channel")

	if err := b.Queries.SetSingAlongChannel(e.Ctx, db.SetSingAlongChannelParams{
		GuildID:          int64(guildID),
		SingAlongChannel: pgtype.Int8{Int64: int64(channel.ID), Valid: true},
	}); err != nil {
		return ephemeralFailure(e, "Configuration Failed",
			fmt.Sprintf("Failed to set the sing-along channel: %s", err.Error()))
	}

	embed := discord.NewEmbed().
		WithTitle("Sing-along Channel Updated").
		WithDescription(fmt.Sprintf("The daily lyric will be posted in <#%d>.", channel.ID)).
		// Said plainly and up front, because it is not recoverable and it is the one
		// thing about this feature somebody could be surprised by.
		AddField("Careful",
			"Every message in that channel is deleted each time a new song drops. "+
				"Do not point this at a channel holding anything worth keeping.", false).
		WithColor(utils.ColorSuccess)

	if config, err := b.Queries.GetGuild(e.Ctx, int64(guildID)); err == nil {
		embed = embed.AddField("Schedule",
			fmt.Sprintf("%02d:00 in `%s`, %d minute cooldown — change it with `/config set-sing-along-time`",
				config.SingAlongHour, config.SingAlongTimezone, config.SingAlongCooldownMinutes), false)
	}

	return e.Respond(discord.InteractionResponseTypeCreateMessage,
		discord.NewMessageCreate().WithEmbeds(embed),
	)
}

func handleSetSingAlongTime(b *stmpdbot.STMPDBot, e *handler.CommandEvent) error {
	data := e.SlashCommandInteractionData()
	guildID := *e.GuildID()

	hour := data.Int("hour")
	timezone := data.String("timezone")

	// Validated before writing, for the same reason as the anniversary schedule: a
	// zone the scheduler cannot resolve leaves the feature silently dead.
	loc, ok := utils.ValidateTimezone(timezone)
	if !ok {
		return ephemeralFailure(e, "Unknown Timezone",
			fmt.Sprintf("`%s` is not an IANA timezone name. Try `Europe/Amsterdam`, "+
				"`America/New_York`, `Asia/Kolkata` or `UTC`.", timezone))
	}

	// The cooldown is optional, so an admin changing only the hour keeps the wait
	// they already chose rather than having it silently reset to the default.
	cooldown := int32(singAlongDefaultCooldown)
	if config, err := b.Queries.GetGuild(e.Ctx, int64(guildID)); err == nil {
		cooldown = config.SingAlongCooldownMinutes
	}
	if minutes, ok := data.OptInt("cooldown"); ok {
		cooldown = int32(minutes)
	}

	if err := b.Queries.SetSingAlongSchedule(e.Ctx, db.SetSingAlongScheduleParams{
		GuildID:                  int64(guildID),
		SingAlongHour:            int32(hour),
		SingAlongTimezone:        timezone,
		SingAlongCooldownMinutes: cooldown,
	}); err != nil {
		return ephemeralFailure(e, "Configuration Failed",
			fmt.Sprintf("Failed to update the sing-along schedule: %s", err.Error()))
	}

	next := nextAnniversaryPost(hour, loc)

	embed := discord.NewEmbed().
		WithTitle("Sing-along Schedule Updated").
		WithDescription(fmt.Sprintf(
			"A new lyric will drop at **%02d:00** in `%s`, with a **%d minute** cooldown between attempts.",
			hour, timezone, cooldown)).
		AddField("Next lyric", fmt.Sprintf("<t:%d:F> (<t:%d:R>)", next.Unix(), next.Unix()), false).
		WithColor(utils.ColorSuccess)

	return e.Respond(discord.InteractionResponseTypeCreateMessage,
		discord.NewMessageCreate().WithEmbeds(embed),
	)
}

func handleSingAlongReroll(b *stmpdbot.STMPDBot, e *handler.CommandEvent) error {
	guildID := *e.GuildID()

	if b.SingAlongReroll == nil {
		return ephemeralFailure(e, "Not Ready Yet",
			"The bot is still starting up. Try again in a moment.")
	}

	song, err := b.SingAlongReroll(e.Ctx, guildID)
	if err != nil {
		return ephemeralFailure(e, "Re-roll Failed", err.Error())
	}

	embed := discord.NewEmbed().
		WithTitle("Sing-along Re-rolled").
		// The song is deliberately not named: the whole game is working out what it
		// is. The channel is what tells the story from here.
		WithDescription("A new song is set. The channel is being cleared, and its first line will appear in a moment.").
		WithColor(utils.ColorSuccess)

	slog.Info("Sing-along re-rolled from a slash command",
		slog.Int64("guild_id", int64(guildID)),
		slog.Int64("song_id", song.ID))

	return e.Respond(discord.InteractionResponseTypeCreateMessage,
		discord.NewMessageCreate().WithEmbeds(embed).WithEphemeral(true),
	)
}

func handleDisableSingAlong(b *stmpdbot.STMPDBot, e *handler.CommandEvent) error {
	guildID := *e.GuildID()

	config, err := b.Queries.GetGuild(e.Ctx, int64(guildID))
	if err != nil {
		return ephemeralFailure(e, "Error", "Failed to fetch server configuration")
	}

	// Only the channel is cleared; the schedule and cooldown stay, so turning it
	// back on remembers what the admin already picked.
	if err := b.Queries.SetSingAlongChannel(e.Ctx, db.SetSingAlongChannelParams{
		GuildID:          int64(guildID),
		SingAlongChannel: pgtype.Int8{},
	}); err != nil {
		return ephemeralFailure(e, "Configuration Failed",
			fmt.Sprintf("Failed to disable the sing-along: %s", err.Error()))
	}

	// The round has to go too, or the listener would keep deleting messages in a
	// channel the feature no longer owns until the next scheduler cycle caught up.
	if err := b.Queries.DeleteSingAlongRound(e.Ctx, int64(guildID)); err != nil {
		slog.Error("Failed to clear the sing-along round", slog.Any("err", err))
	}

	if config.SingAlongChannel.Valid {
		channelID := snowflake.ID(config.SingAlongChannel.Int64)
		b.SingAlong.Clear(channelID)

		// Slowmode belongs to the feature, not to the channel, so a channel handed
		// back is handed back unthrottled.
		noSlowmode := 0
		if _, err := b.Client.Rest.UpdateChannel(channelID,
			discord.GuildTextChannelUpdate{RateLimitPerUser: &noSlowmode}); err != nil {
			slog.Warn("Failed to clear sing-along slowmode", slog.Any("err", err))
		}
	}

	embed := discord.NewEmbed().
		WithTitle("Sing-along Disabled").
		WithDescription("No more daily lyrics will be posted, and the channel's slowmode has been cleared. " +
			"Your time, timezone and cooldown have been kept for when you turn it back on.").
		WithColor(utils.ColorSuccess)

	return e.Respond(discord.InteractionResponseTypeCreateMessage,
		discord.NewMessageCreate().WithEmbeds(embed),
	)
}

// ConfigAutocompleteHandler serves the timezone suggestions on
// /config set-anniversary-time. It is registered against "/config" because the
// handler mux matches on the pattern's path segments, so one registration covers
// every subcommand -- the same reason rootHandler.Command("/config", ...) does.
func ConfigAutocompleteHandler(b *stmpdbot.STMPDBot) handler.AutocompleteHandler {
	return func(e *handler.AutocompleteEvent) error {
		focused := e.Data.Focused()
		if focused.Name != "timezone" {
			return e.AutocompleteResult(nil)
		}

		return e.AutocompleteResult(utils.FilterTimezones(e.Data.String("timezone")))
	}
}

func handleViewConfig(b *stmpdbot.STMPDBot, e *handler.CommandEvent) error {
	guildID := *e.GuildID()

	// Get guild configuration
	config, err := b.Queries.GetGuild(e.Ctx, int64(guildID))
	if err != nil {
		return e.Respond(discord.InteractionResponseTypeCreateMessage,
			discord.NewMessageCreate().
				WithEmbeds(utils.FailureEmbed("Error", "Failed to fetch server configuration")).
				WithEphemeral(true),
		)
	}

	embed := discord.NewEmbed().
		WithTitle("Server Configuration").
		WithColor(utils.ColorInfo)

	// Moderator Role
	if config.ModeratorRole.Valid {
		embed = embed.AddField("Moderator Role", fmt.Sprintf("<@&%d>", config.ModeratorRole.Int64), true)
	} else {
		embed = embed.AddField("Moderator Role", "Not set (using default permissions)", true)
	}

	// Modlogs Channel
	if config.ModlogsChannel.Valid {
		embed = embed.AddField("Moderation Logs Channel", fmt.Sprintf("<#%d>", config.ModlogsChannel.Int64), true)
	} else {
		embed = embed.AddField("Moderation Logs Channel", "Not set", true)
	}

	// Bot Channel -- where level-up announcements go. Unset turns them off.
	if config.BotChannel.Valid {
		embed = embed.AddField("Level-up Channel", fmt.Sprintf("<#%d>", config.BotChannel.Int64), true)
	} else {
		embed = embed.AddField("Level-up Channel", "Not set", true)
	}

	// Radio Voice Channel
	if config.RadioVoiceChannel.Valid {
		embed = embed.AddField("Radio Voice Channel", fmt.Sprintf("<#%d>", config.RadioVoiceChannel.Int64), true)
	} else {
		embed = embed.AddField("Radio Voice Channel", "Not set", true)
	}

	// XP Multiplier
	embed = embed.AddField("XP Multiplier", fmt.Sprintf("%.1fx", config.XpMultiplier), true)

	// Level-up role, and the level it is granted at.
	if config.LevelUpRole.Valid {
		embed = embed.AddField("Level-up Role",
			fmt.Sprintf("<@&%d> at level %d", config.LevelUpRole.Int64, config.LevelUpRoleLevel), true)
	} else {
		embed = embed.AddField("Level-up Role", "Not set", true)
	}

	// Notifications section
	notificationsText := ""

	if config.YoutubeNotificationsChannel.Valid {
		notificationsText += fmt.Sprintf("**YouTube:** <#%d>", config.YoutubeNotificationsChannel.Int64)
		if config.YoutubeNotificationsRole.Valid {
			notificationsText += fmt.Sprintf(" (<@&%d>)", config.YoutubeNotificationsRole.Int64)
		}
		notificationsText += "\n"
	}

	if config.RedditNotificationsChannel.Valid {
		notificationsText += fmt.Sprintf("**Reddit:** <#%d>", config.RedditNotificationsChannel.Int64)
		if config.RedditNotificationsRole.Valid {
			notificationsText += fmt.Sprintf(" (<@&%d>)", config.RedditNotificationsRole.Int64)
		}
		notificationsText += "\n"
	}

	if config.StmpdNotificationsChannel.Valid {
		notificationsText += fmt.Sprintf("**STMPD:** <#%d>", config.StmpdNotificationsChannel.Int64)
		if config.StmpdNotificationsRole.Valid {
			notificationsText += fmt.Sprintf(" (<@&%d>)", config.StmpdNotificationsRole.Int64)
		}
		notificationsText += "\n"
	}

	if config.TourNotificationsChannel.Valid {
		notificationsText += fmt.Sprintf("**Tour:** <#%d>", config.TourNotificationsChannel.Int64)
		if config.TourNotificationsRole.Valid {
			notificationsText += fmt.Sprintf(" (<@&%d>)", config.TourNotificationsRole.Int64)
		}
		notificationsText += "\n"
	}

	if config.AnniversaryNotificationsChannel.Valid {
		notificationsText += fmt.Sprintf("**Anniversaries:** <#%d>", config.AnniversaryNotificationsChannel.Int64)
		if config.AnniversaryNotificationsRole.Valid {
			notificationsText += fmt.Sprintf(" (<@&%d>)", config.AnniversaryNotificationsRole.Int64)
		}
		notificationsText += fmt.Sprintf(" — daily at %02d:00 `%s`\n",
			config.AnniversaryHour, config.AnniversaryTimezone)
	}

	if notificationsText == "" {
		notificationsText = "No notification channels configured"
	}

	embed = embed.AddField("Notification Channels", notificationsText, false)

	singAlongText := "Not configured"
	if config.SingAlongChannel.Valid {
		singAlongText = fmt.Sprintf("<#%d> — daily at %02d:00 `%s`, %d minute cooldown",
			config.SingAlongChannel.Int64, config.SingAlongHour,
			config.SingAlongTimezone, config.SingAlongCooldownMinutes)
	}
	embed = embed.AddField("Sing-along", singAlongText, false)

	return e.Respond(discord.InteractionResponseTypeCreateMessage,
		discord.NewMessageCreate().
			WithEmbeds(embed).
			WithEphemeral(true),
	)
}
