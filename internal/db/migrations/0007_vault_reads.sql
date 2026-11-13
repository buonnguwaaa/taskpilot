-- taskpilot schema v7: audit of the agents' reads of the QC Vault (MCP tools
-- qc_config_list / qc_config_get). Values are never stored, only the path,
-- the action, how many keys came back, and the error.
CREATE TABLE vault_reads (
    id       INTEGER PRIMARY KEY,
    task_id  TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    at       TEXT NOT NULL,
    action   TEXT NOT NULL CHECK (action IN ('list', 'get')),
    path     TEXT NOT NULL,
    keys     INTEGER NOT NULL DEFAULT 0,
    error    TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX vault_reads_task ON vault_reads (task_id);
