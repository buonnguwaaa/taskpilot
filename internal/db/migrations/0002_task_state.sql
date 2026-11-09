-- taskpilot schema v2: every piece of task state moves from files into SQLite.
-- The v1 work-tracking tables were never written (task state lived in files),
-- so they are rebuilt to the current model: projects + workspaces, PR flow
-- stages, baselines, takeovers, Splunk audit. Secrets are untouched.
-- Large append-only blobs stay on disk (events.jsonl, stderr logs) and are
-- referenced by path.

DROP VIEW IF EXISTS v_gate_stats;
DROP VIEW IF EXISTS v_inbox;
DROP VIEW IF EXISTS v_denials;
DROP VIEW IF EXISTS v_cost_daily;

DROP TABLE IF EXISTS eval_results;
DROP TABLE IF EXISTS run_tokens;
DROP TABLE IF EXISTS tool_calls;
DROP TABLE IF EXISTS runs;
DROP TABLE IF EXISTS approvals;
DROP TABLE IF EXISTS operator_messages;
DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS task_transitions;
DROP TABLE IF EXISTS tasks;

CREATE TABLE tasks (
    id           TEXT PRIMARY KEY,
    source       TEXT NOT NULL DEFAULT '',
    ticket_id    TEXT NOT NULL DEFAULT '',
    ticket_url   TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL DEFAULT '',
    project      TEXT NOT NULL DEFAULT '',
    repo_hint    TEXT NOT NULL DEFAULT '',
    branch       TEXT NOT NULL DEFAULT '',
    workspace    TEXT NOT NULL DEFAULT '',
    session_id   TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL CHECK (state IN
                   ('queued','preparing','running','awaiting_approval','needs_attention','done','failed')),
    next_prompt  TEXT NOT NULL DEFAULT '',
    next_trigger TEXT NOT NULL DEFAULT '',
    runs_count   INTEGER NOT NULL DEFAULT 0,
    cost_usd     REAL NOT NULL DEFAULT 0,
    last_result  TEXT NOT NULL DEFAULT '',
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    closed_at    TEXT
) STRICT;

CREATE INDEX tasks_state ON tasks (state, created_at);

CREATE TABLE task_transitions (
    id       INTEGER PRIMARY KEY,
    task_id  TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    at       TEXT NOT NULL,
    state    TEXT NOT NULL,
    note     TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX task_transitions_task ON task_transitions (task_id, id);

-- Approval requests raised by the agent (MCP) and the operator's decisions (TUI/CLI).
CREATE TABLE approvals (
    task_id     TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    id          TEXT NOT NULL,                 -- "<gate>-<attempt>"
    gate        TEXT NOT NULL,
    attempt     INTEGER NOT NULL,
    summary     TEXT NOT NULL,
    details     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    decision    TEXT CHECK (decision IN ('approved','rejected')),   -- NULL = pending
    comment     TEXT NOT NULL DEFAULT '',
    decided_by  TEXT NOT NULL DEFAULT '',
    decided_at  TEXT,
    PRIMARY KEY (task_id, id),
    UNIQUE (task_id, gate, attempt),
    CHECK ((decision IS NULL) = (decided_at IS NULL))
) STRICT;

CREATE UNIQUE INDEX approvals_one_pending ON approvals (task_id) WHERE decision IS NULL;

CREATE TABLE completions (
    id           INTEGER PRIMARY KEY,
    task_id      TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    summary      TEXT NOT NULL,
    pr_url       TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    archived_at  TEXT                         -- set when the task is reopened
) STRICT;

CREATE UNIQUE INDEX completions_current ON completions (task_id) WHERE archived_at IS NULL;

CREATE TABLE operator_messages (
    id           INTEGER PRIMARY KEY,
    task_id      TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    message      TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    consumed_at  TEXT
) STRICT;

CREATE INDEX operator_messages_pending ON operator_messages (task_id) WHERE consumed_at IS NULL;

-- The operator drives the session by hand (taskpilot takeover); pid detects stale rows.
CREATE TABLE takeovers (
    task_id     TEXT PRIMARY KEY REFERENCES tasks (id) ON DELETE CASCADE,
    pid         INTEGER NOT NULL,
    started_at  TEXT NOT NULL
) STRICT;

-- Verify result of a repo on the untouched base, recorded when the repo is opened.
CREATE TABLE baselines (
    task_id      TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    repo         TEXT NOT NULL,
    command      TEXT NOT NULL,
    passed       INTEGER NOT NULL CHECK (passed IN (0,1)),
    output       TEXT NOT NULL DEFAULT '',
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    at           TEXT NOT NULL,
    PRIMARY KEY (task_id, repo)
) STRICT;

-- One PR per repo and target branch (release/qc, then main).
CREATE TABLE pull_requests (
    task_id      TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    repo         TEXT NOT NULL,
    destination  TEXT NOT NULL,
    pr_id        INTEGER NOT NULL DEFAULT 0,
    url          TEXT NOT NULL,
    source       TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    pending      INTEGER NOT NULL DEFAULT 0 CHECK (pending IN (0,1)),   -- pushed, PR page not opened yet
    created_at   TEXT NOT NULL,
    PRIMARY KEY (task_id, repo, destination)
) STRICT;

CREATE TABLE splunk_queries (
    id        INTEGER PRIMARY KEY,
    task_id   TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    at        TEXT NOT NULL,
    query     TEXT NOT NULL,
    earliest  TEXT NOT NULL DEFAULT '',
    latest    TEXT NOT NULL DEFAULT '',
    events    INTEGER NOT NULL DEFAULT 0,
    error     TEXT NOT NULL DEFAULT ''
) STRICT;

-- One row per `claude -p` invocation.
CREATE TABLE runs (
    id                 INTEGER PRIMARY KEY,
    task_id            TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    n                  INTEGER NOT NULL,
    session_id         TEXT NOT NULL,
    resume             INTEGER NOT NULL CHECK (resume IN (0,1)),
    trigger            TEXT NOT NULL CHECK (trigger IN ('initial','approval','rejection','operator_message')),
    prompt             TEXT NOT NULL,
    model              TEXT NOT NULL DEFAULT '',
    effort             TEXT NOT NULL DEFAULT '',
    started_at         TEXT NOT NULL,
    ended_at           TEXT,
    duration_ms        INTEGER,
    result_subtype     TEXT NOT NULL DEFAULT '',
    is_error           INTEGER NOT NULL DEFAULT 0 CHECK (is_error IN (0,1)),
    error              TEXT NOT NULL DEFAULT '',
    num_turns          INTEGER NOT NULL DEFAULT 0,
    cost_usd           REAL NOT NULL DEFAULT 0,
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    outcome            TEXT CHECK (outcome IN ('awaiting_approval','done','needs_attention','failed','interrupted')),
    events_path        TEXT NOT NULL DEFAULT '',
    stderr_path        TEXT NOT NULL DEFAULT '',
    UNIQUE (task_id, n)
) STRICT;

CREATE INDEX runs_started ON runs (started_at);

CREATE TABLE tool_calls (
    id             INTEGER PRIMARY KEY,
    run_id         INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq            INTEGER NOT NULL,
    tool_use_id    TEXT NOT NULL DEFAULT '',
    tool_name      TEXT NOT NULL,
    input_summary  TEXT NOT NULL DEFAULT '',   -- truncated and redacted
    is_error       INTEGER NOT NULL DEFAULT 0 CHECK (is_error IN (0,1)),
    denied_by      TEXT CHECK (denied_by IN ('hook','permission')),
    deny_reason    TEXT NOT NULL DEFAULT '',
    UNIQUE (run_id, seq)
) STRICT;

CREATE INDEX tool_calls_name ON tool_calls (tool_name);
CREATE INDEX tool_calls_denied ON tool_calls (denied_by) WHERE denied_by IS NOT NULL;

-- Reports to the ticket system, retried until delivered.
CREATE TABLE outbox (
    id               INTEGER PRIMARY KEY,
    task_id          TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    source           TEXT NOT NULL,
    ticket_id        TEXT NOT NULL,
    message          TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','failed')),
    attempts         INTEGER NOT NULL DEFAULT 0,
    last_error       TEXT NOT NULL DEFAULT '',
    next_attempt_at  TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    sent_at          TEXT
) STRICT;

CREATE INDEX outbox_due ON outbox (next_attempt_at) WHERE status = 'pending';

CREATE TABLE eval_results (
    id             INTEGER PRIMARY KEY,
    case_id        TEXT NOT NULL REFERENCES eval_cases (id),
    task_id        TEXT REFERENCES tasks (id) ON DELETE SET NULL,
    repetition     INTEGER NOT NULL,
    model          TEXT,
    skill_version  TEXT,
    score          REAL,
    passed         INTEGER CHECK (passed IN (0,1)),
    skill_loaded   INTEGER CHECK (skill_loaded IN (0,1)),
    cost_usd       REAL,
    duration_ms    INTEGER,
    notes          TEXT,
    created_at     TEXT NOT NULL
) STRICT;

CREATE TABLE run_tokens (
    token_hash     TEXT PRIMARY KEY,
    run_id         INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    consumer_kind  TEXT NOT NULL,
    consumer_name  TEXT NOT NULL,
    expires_at     TEXT NOT NULL,
    consumed_at    TEXT
) STRICT;

CREATE VIEW v_gate_stats AS
SELECT gate,
       count(*)                                          AS requests,
       count(*) FILTER (WHERE decision = 'approved')     AS approved,
       count(*) FILTER (WHERE decision = 'rejected')     AS rejected,
       count(*) FILTER (WHERE decision IS NULL)          AS pending,
       round(1.0 * count(*) FILTER (WHERE decision = 'approved')
                 / nullif(count(*) FILTER (WHERE decision IS NOT NULL), 0), 3) AS approval_rate,
       round(avg((julianday(decided_at) - julianday(created_at)) * 1440), 1)    AS avg_wait_minutes
FROM approvals
GROUP BY gate;

CREATE VIEW v_inbox AS
SELECT t.id AS task_id, t.title, t.project, t.state, a.id AS request_id, a.gate, a.summary,
       coalesce(a.created_at, t.updated_at) AS waiting_since
FROM tasks t
LEFT JOIN approvals a ON a.task_id = t.id AND a.decision IS NULL
WHERE t.state IN ('awaiting_approval','needs_attention','failed')
  AND NOT EXISTS (SELECT 1 FROM takeovers k WHERE k.task_id = t.id)
ORDER BY waiting_since;

CREATE VIEW v_denials AS
SELECT tc.tool_name, tc.denied_by, tc.deny_reason, count(*) AS n, max(r.started_at) AS last_seen
FROM tool_calls tc JOIN runs r ON r.id = tc.run_id
WHERE tc.denied_by IS NOT NULL
GROUP BY tc.tool_name, tc.denied_by, tc.deny_reason
ORDER BY n DESC;

CREATE VIEW v_cost_daily AS
SELECT date(r.started_at) AS day, t.project, count(*) AS runs, round(sum(r.cost_usd), 2) AS cost_usd
FROM runs r JOIN tasks t ON t.id = r.task_id
GROUP BY day, t.project
ORDER BY day DESC, cost_usd DESC;
