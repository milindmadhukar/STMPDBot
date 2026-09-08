-- The nightly memory digest's watermark.
--
-- The digest (cmd/agent/digest.go) sweeps messages the bot was never addressed
-- in and folds what is worth keeping into long-term memory. It needs to know
-- where it stopped last time, and it cannot keep that in the agent's own
-- process: that container redeploys on every push, and a watermark that reset
-- on restart would either re-ingest the same days repeatedly -- paying for
-- every one -- or skip whatever landed while it was down.
--
-- One row, enforced by the primary key. There is only ever one digest.
CREATE TABLE IF NOT EXISTS agent_memory_digest (
    id           smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    last_run_at  timestamptz NOT NULL,
    -- The high-water mark of message timestamps actually consumed, which is
    -- not the same as when the pass ran: a run that finds nothing still
    -- happened, and the next one must not re-read the window it covered.
    watermark    timestamptz NOT NULL,
    messages_seen integer NOT NULL DEFAULT 0,
    memories_written integer NOT NULL DEFAULT 0
);
