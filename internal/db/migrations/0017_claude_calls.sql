-- taskpilot schema v17: token usage of the one-turn `claude -p` calls that
-- are not task runs (drafting a task, work logs, assessing a task, grouping
-- errors, writing SPL, reflect, merge intent). Task runs keep theirs in runs.
CREATE TABLE claude_calls (
    id                 INTEGER PRIMARY KEY,
    at                 TEXT NOT NULL,
    purpose            TEXT NOT NULL DEFAULT '',
    model              TEXT NOT NULL DEFAULT '',
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd           REAL NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX claude_calls_at ON claude_calls (at);
