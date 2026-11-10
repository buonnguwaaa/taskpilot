-- taskpilot schema v4: audit of the agents' read-only QC database queries
-- (MCP tool db_query), like splunk_queries.
CREATE TABLE db_queries (
    id         INTEGER PRIMARY KEY,
    task_id    TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    at         TEXT NOT NULL,
    target     TEXT NOT NULL,          -- host:port/schema, no credentials
    query      TEXT NOT NULL,
    rows       INTEGER NOT NULL DEFAULT 0,
    error      TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX db_queries_task ON db_queries (task_id);
