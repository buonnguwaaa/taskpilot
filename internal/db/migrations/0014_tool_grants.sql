-- taskpilot schema v14: tools the operator allowed while a task ran. An
-- agent denied a command outside the allowlist asks for it
-- (request_permission); the operator allows it for the task or for every
-- task of the project, and the gate hook lets matching calls through.
CREATE TABLE tool_grants (
    scope       TEXT NOT NULL CHECK (scope IN ('task', 'project')),
    target      TEXT NOT NULL,               -- task id or project name
    rule        TEXT NOT NULL,               -- Claude Code permission rule, e.g. Bash(curl *)
    created_at  TEXT NOT NULL,
    PRIMARY KEY (scope, target, rule)
) STRICT;

-- The rule a permission request asks for (the request itself is a question
-- in approvals).
CREATE TABLE permission_requests (
    task_id     TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    request_id  TEXT NOT NULL,               -- per task (question-1, ...)
    rule        TEXT NOT NULL,
    PRIMARY KEY (task_id, request_id)
) STRICT;
