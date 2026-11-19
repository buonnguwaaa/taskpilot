-- taskpilot schema v15: tasks the operator put on hold (TUI key z). A row
-- here moves a parked task to the "Tạm hoãn" group of the list and out of
-- the "cần bạn" count; the agent is not touched. The daemon drops the row
-- when the task runs again or is done (store.SaveTask).
CREATE TABLE task_postponed (
    task_id     TEXT PRIMARY KEY REFERENCES tasks (id) ON DELETE CASCADE,
    note        TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
) STRICT;
