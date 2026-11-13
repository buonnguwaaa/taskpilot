-- taskpilot schema v9: error flows. The groups of a scan that belong to one
-- failing path (shared request ids / transaction codes, or joined by Claude)
-- are one flow; the operator acts on the flow.
CREATE TABLE error_flows (
    id         INTEGER PRIMARY KEY,
    project    TEXT NOT NULL,
    scan_id    INTEGER,
    title      TEXT NOT NULL DEFAULT '',
    cause      TEXT NOT NULL DEFAULT '',
    how        TEXT NOT NULL DEFAULT '',   -- request_id, claude
    created_at TEXT NOT NULL
) STRICT;

ALTER TABLE error_suggestions ADD COLUMN flow_id INTEGER;
