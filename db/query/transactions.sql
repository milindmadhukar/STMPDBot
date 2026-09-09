-- name: AddCoins :exec
UPDATE users SET in_hand=in_hand + $3 WHERE id = $1 AND guild_id = $2;

-- name: GetBalance :one
SELECT stmpd_coins, in_hand FROM users WHERE id = $1 AND guild_id = $2;

-- name: WithdrawAmount :exec
UPDATE users SET in_hand = in_hand + $3, stmpd_coins = stmpd_coins - $3 WHERE id = $1 AND guild_id = $2;

-- name: DepositAmount :exec
UPDATE users SET in_hand = in_hand - $3, stmpd_coins = stmpd_coins + $3 WHERE id = $1 AND guild_id = $2;

-- name: GiveCoins :exec
WITH sender_update AS (
    UPDATE users AS sender
    SET in_hand = sender.in_hand - $4
    WHERE sender.id = $1 AND sender.in_hand >= $4 AND sender.guild_id = $3
    RETURNING sender.id
)
UPDATE users AS receiver
SET in_hand = receiver.in_hand + $4
WHERE receiver.id = $2 AND receiver.guild_id = $3
AND EXISTS (SELECT 1 FROM sender_update);

-- TODO: Use sqlc.arg for argument names
-- name: AwardCoins :execrows
-- AddCoins, but it says whether it actually paid anybody.
--
-- The UPDATE matches on (id, guild_id), so a member with no users row is silently
-- passed over -- fine for the paths that create the row first, not fine for the
-- sing-along, where the award is the reward for playing and losing it quietly is the
-- worst outcome. The caller creates the row and retries on 0.
UPDATE users
SET in_hand = in_hand + $3
WHERE id = $1 AND guild_id = $2;
