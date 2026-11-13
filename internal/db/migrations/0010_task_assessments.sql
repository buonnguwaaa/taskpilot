-- taskpilot schema v10: the agent's assessment of a task (difficulty level
-- and kind of work), shown in the TUI. Kept apart from the execution profile:
-- the operator pinning the model does not stop the agent from assessing.
CREATE TABLE task_assessments (
    task_id     TEXT PRIMARY KEY REFERENCES tasks (id) ON DELETE CASCADE,
    level       TEXT NOT NULL DEFAULT '',   -- light, standard, hard, hardest
    kind        TEXT NOT NULL DEFAULT '',   -- feature, bugfix, investigation, ...
    reason      TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL
) STRICT;
