DROP TABLE IF EXISTS sing_along_rounds;
ALTER TABLE guilds DROP COLUMN IF EXISTS sing_along_cooldown_minutes;
ALTER TABLE guilds DROP COLUMN IF EXISTS sing_along_timezone;
ALTER TABLE guilds DROP COLUMN IF EXISTS sing_along_hour;
ALTER TABLE guilds DROP COLUMN IF EXISTS sing_along_channel;
