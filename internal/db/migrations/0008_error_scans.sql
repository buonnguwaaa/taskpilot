-- taskpilot schema v8: scheduled production error scans and the fix
-- suggestions they leave for the operator (one per project, service and
-- message signature; recurring errors update the same suggestion).
CREATE TABLE error_scans (
    id           INTEGER PRIMARY KEY,
    project      TEXT NOT NULL,
    started_at   TEXT NOT NULL,
    finished_at  TEXT,
    earliest     TEXT NOT NULL,
    latest       TEXT NOT NULL,
    groups       INTEGER NOT NULL DEFAULT 0,
    new          INTEGER NOT NULL DEFAULT 0,   -- suggestions created or re-opened
    error        TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX error_scans_project ON error_scans (project, started_at);

CREATE TABLE error_suggestions (
    id           INTEGER PRIMARY KEY,
    project      TEXT NOT NULL,
    service      TEXT NOT NULL,
    signature    TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'new' CHECK (status IN ('new', 'dismissed', 'snoozed', 'task')),
    snooze_until TEXT,
    task_id      TEXT,                      -- the task created from it
    count        INTEGER NOT NULL DEFAULT 0, -- total over all scans
    last_count   INTEGER NOT NULL DEFAULT 0, -- in the latest scan that saw it
    first_seen   TEXT NOT NULL,
    last_seen    TEXT NOT NULL,
    sample       TEXT NOT NULL DEFAULT '',
    caller       TEXT NOT NULL DEFAULT '',
    prefix       TEXT NOT NULL DEFAULT '',
    request_id   TEXT NOT NULL DEFAULT '',
    stacktrace   TEXT NOT NULL DEFAULT '',
    scan_id      INTEGER,                   -- latest scan that saw it
    UNIQUE (project, service, signature)
) STRICT;
