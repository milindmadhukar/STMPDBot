-- name: GetRedditNotificationChannels :many
SELECT reddit_notifications_channel, reddit_notifications_role 
FROM guilds
WHERE reddit_notifications_channel IS NOT NULL;

-- name: GetYoutubeNotifactionChannels :many
SELECT youtube_notifications_channel, youtube_notifications_role 
FROM guilds
WHERE youtube_notifications_channel IS NOT NULL;

-- name: GetSTMPDNofiticationChannels :many
-- guild_id comes back so a release announcement can be recorded against the guild it
-- landed in; song_announcements is keyed on (guild_id, message_id).
SELECT guild_id, stmpd_notifications_channel, stmpd_notifications_role
FROM guilds
WHERE stmpd_notifications_channel IS NOT NULL;

-- name: GetRadioVoiceChannels :many
SELECT guild_id, radio_voice_channel
FROM guilds
WHERE radio_voice_channel IS NOT NULL;

-- name: SetModeratorRole :exec
UPDATE guilds
SET moderator_role = $2
WHERE guild_id = $1;

-- name: CreateGuild :one
INSERT INTO guilds(guild_id)
VALUES ($1)
ON CONFLICT (guild_id) DO NOTHING
RETURNING *;

-- name: GetGuild :one
SELECT * FROM guilds WHERE guild_id = $1;

-- name: GetModlogsChannel :one
-- sendModlogToChannel used to SELECT * on guilds just to read one column, on
-- every moderation action.
SELECT modlogs_channel FROM guilds WHERE guild_id = $1;

-- name: GetVoiceLogsChannel :one
SELECT voice_logs_channel FROM guilds WHERE guild_id = $1;

-- name: GetMemberLogsChannel :one
SELECT member_logs_channel FROM guilds WHERE guild_id = $1;

-- name: UpdateGuildConfig :one
-- A full-row update rather than per-field setters or COALESCE.
--
-- The settings form always submits every field, so the query mirrors the form:
-- whatever the admin is looking at is what gets written. COALESCE($n, col)
-- cannot express CLEARING a setting -- it reads a NULL parameter as "leave
-- alone" -- and every column here is nullable precisely so a channel can be
-- unset. Unsetting is a first-class action, not an edge case.
--
-- RETURNING * so the handler re-renders what the database actually holds rather
-- than echoing back what the browser sent.
UPDATE guilds SET
    modlogs_channel                 = $2,
    leave_join_logs_channel         = $3,
    delete_logs_channel             = $4,
    edit_logs_channel               = $5,
    voice_logs_channel              = $6,
    member_logs_channel             = $7,
    welcomes_channel                = $8,
    youtube_notifications_channel   = $9,
    youtube_notifications_role      = $10,
    reddit_notifications_channel    = $11,
    reddit_notifications_role       = $12,
    stmpd_notifications_channel     = $13,
    stmpd_notifications_role        = $14,
    tour_notifications_channel      = $15,
    tour_notifications_role         = $16,
    anniversary_notifications_channel = $17,
    anniversary_notifications_role  = $18,
    anniversary_hour                = $19,
    anniversary_timezone            = $20,
    moderator_role                  = $21,
    news_role                       = $22,
    bot_channel                     = $23,
    radio_voice_channel             = $24,
    xp_multiplier                   = $25,
    level_up_role                   = $26,
    level_up_role_level             = $27,
    sing_along_channel              = $28,
    sing_along_hour                 = $29,
    sing_along_timezone             = $30,
    sing_along_cooldown_minutes     = $31
WHERE guild_id = $1
RETURNING *;

-- name: GetDeleteLogRouting :one
-- Where deletions are logged, and which channel to stay out of, in one round trip.
--
-- The delete-log listener needs both because the sing-along deletes a wrong guess on
-- every attempt and empties its channel once a day. disgo fans MESSAGE_DELETE_BULK
-- out into one GuildMessageDelete per message, so without this a single daily wipe
-- would post several hundred "Message Deleted" embeds into the moderation log.
SELECT delete_logs_channel, sing_along_channel FROM guilds WHERE guild_id = $1;
