-- taskpilot schema v13: more tickets worked in a task. A bug found on a
-- task's feature is often its own ticket; the operator attaches it to the
-- task (TUI Hasaki Work tab, key a) so one agent session and one branch do
-- both. The task's own ticket stays tasks.ticket_id.
CREATE TABLE task_tickets (
    task_id     TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    source      TEXT NOT NULL,
    ticket_id   TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    PRIMARY KEY (source, ticket_id)
) STRICT;
CREATE INDEX task_tickets_task ON task_tickets (task_id);
