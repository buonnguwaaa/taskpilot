-- taskpilot schema v5: the execution profile (model, effort) of a task, chosen
-- by the agent after assessing the task (MCP tool set_execution_profile) or
-- pinned by the operator, who always wins. Applies from the next run.
CREATE TABLE task_profiles (
    task_id     TEXT PRIMARY KEY REFERENCES tasks (id) ON DELETE CASCADE,
    model       TEXT NOT NULL DEFAULT '',
    effort      TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    set_by      TEXT NOT NULL CHECK (set_by IN ('agent', 'operator')),
    updated_at  TEXT NOT NULL
) STRICT;
