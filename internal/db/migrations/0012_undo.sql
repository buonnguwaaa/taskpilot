-- taskpilot schema v12: the operator takes back a message or decision.
-- A taken-back run is marked revoked; the next run resumes the agent's
-- session at the entry before it (claude --resume-session-at).
ALTER TABLE runs ADD COLUMN resume_at TEXT NOT NULL DEFAULT '';   -- session entry this run continued from ('' = unknown)
ALTER TABLE runs ADD COLUMN revoked_at TEXT;                      -- the operator took back what started it

CREATE TABLE task_undo (
    task_id      TEXT PRIMARY KEY REFERENCES tasks (id) ON DELETE CASCADE,
    requested_at TEXT,                    -- take back the latest run's message/decision (done by the daemon)
    resume_at    TEXT NOT NULL DEFAULT '' -- the next run resumes the session at this entry
) STRICT;
