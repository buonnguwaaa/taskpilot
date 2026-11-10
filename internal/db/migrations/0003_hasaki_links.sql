-- taskpilot schema v3: a local task pushed to Hasaki Work as a new task
-- (TUI key h): the detail pane shows its code; a re-push replaces it.
CREATE TABLE hasaki_links (
    task_id     TEXT PRIMARY KEY REFERENCES tasks (id) ON DELETE CASCADE,
    hw_id       INTEGER NOT NULL,
    code        TEXT NOT NULL,
    created_at  TEXT NOT NULL
) STRICT;
