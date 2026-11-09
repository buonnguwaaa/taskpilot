-- taskpilot schema v1. Applied when PRAGMA user_version < 1.
-- Conventions: timestamps are TEXT in RFC 3339 UTC; booleans are INTEGER 0/1;
-- every table is STRICT so a wrong type fails loudly instead of being coerced.
-- Connection settings (set by the app, not here): journal_mode=WAL,
-- busy_timeout=5000, foreign_keys=ON.

------------------------------------------------------------------------------
-- Work tracking
------------------------------------------------------------------------------

CREATE TABLE tasks (
    id                TEXT PRIMARY KEY,                 -- "<source>-<ticket_id>", filesystem-safe
    source            TEXT NOT NULL,                    -- mock | hasaki-work | ...
    ticket_id         TEXT NOT NULL,
    ticket_url        TEXT,
    title             TEXT NOT NULL,
    body              TEXT NOT NULL DEFAULT '',
    repo              TEXT NOT NULL,
    branch            TEXT,
    worktree          TEXT,
    base_sha          TEXT,                             -- commit the worktree started from
    head_sha          TEXT,                             -- last commit seen after a run
    session_id        TEXT NOT NULL,                    -- Claude Code session, fixed for the task
    state             TEXT NOT NULL CHECK (state IN
                        ('queued','preparing','running','awaiting_approval',
                         'needs_attention','done','failed')),
    next_prompt       TEXT,                             -- what the next run sends; NULL = initial prompt
    last_result       TEXT,
    last_error        TEXT,
    pr_url            TEXT,
    completion_summary TEXT,
    taken_over_at     TEXT,                             -- operator drives the session; daemon skips it
    taken_over_by     TEXT,
    runs_count        INTEGER NOT NULL DEFAULT 0,
    cost_usd          REAL    NOT NULL DEFAULT 0,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    closed_at         TEXT,                             -- set when state becomes done/failed
    UNIQUE (source, ticket_id)
) STRICT;

CREATE INDEX tasks_state ON tasks (state, created_at);

CREATE TABLE task_transitions (
    id          INTEGER PRIMARY KEY,
    task_id     TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    at          TEXT NOT NULL,
    from_state  TEXT,
    to_state    TEXT NOT NULL,
    actor       TEXT NOT NULL CHECK (actor IN ('daemon','operator','agent')),
    note        TEXT
) STRICT;

CREATE INDEX task_transitions_task ON task_transitions (task_id, at);

-- One row per `claude -p` invocation (one turn between two gates).
CREATE TABLE runs (
    id                 INTEGER PRIMARY KEY,
    task_id            TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    n                  INTEGER NOT NULL,                -- 1-based within the task
    session_id         TEXT NOT NULL,
    resume             INTEGER NOT NULL CHECK (resume IN (0,1)),
    trigger            TEXT NOT NULL CHECK (trigger IN ('initial','approval','rejection','operator_message')),
    prompt             TEXT NOT NULL,
    model              TEXT,
    effort             TEXT,
    skill_version      TEXT,                            -- sha256 of the workflow skill as shipped
    taskpilot_version  TEXT,
    claude_version     TEXT,
    started_at         TEXT NOT NULL,
    ended_at           TEXT,
    duration_ms        INTEGER,
    exit_code          INTEGER,
    result_subtype     TEXT,                            -- success | error_max_budget_usd | ...
    is_error           INTEGER CHECK (is_error IN (0,1)),
    num_turns          INTEGER,
    cost_usd           REAL,
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    cache_read_tokens  INTEGER,
    cache_write_tokens INTEGER,
    outcome            TEXT CHECK (outcome IN ('awaiting_approval','done','needs_attention','interrupted')),
    events_path        TEXT,                            -- raw stream-json stays on disk
    stderr_path        TEXT,
    UNIQUE (task_id, n)
) STRICT;

CREATE INDEX runs_started ON runs (started_at);

-- Requests raised by the agent and the operator's decision, one row each.
CREATE TABLE approvals (
    id           TEXT NOT NULL,                         -- "<gate>-<attempt>", e.g. plan-2
    task_id      TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    gate         TEXT NOT NULL,
    attempt      INTEGER NOT NULL,
    run_id       INTEGER REFERENCES runs (id),          -- run that raised it
    summary      TEXT NOT NULL,
    details      TEXT,
    created_at   TEXT NOT NULL,
    decision     TEXT CHECK (decision IN ('approved','rejected')),   -- NULL = pending
    comment      TEXT,
    decided_by   TEXT,
    decided_at   TEXT,
    PRIMARY KEY (task_id, id),
    UNIQUE (task_id, gate, attempt),
    CHECK ((decision IS NULL) = (decided_at IS NULL))
) STRICT;

-- The database itself guarantees at most one pending request per task.
CREATE UNIQUE INDEX approvals_one_pending ON approvals (task_id) WHERE decision IS NULL;
CREATE INDEX approvals_pending ON approvals (created_at) WHERE decision IS NULL;

-- Tool calls extracted from the event stream, for denial and skill-usage analysis.
CREATE TABLE tool_calls (
    id             INTEGER PRIMARY KEY,
    run_id         INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq            INTEGER NOT NULL,
    tool_use_id    TEXT,
    tool_name      TEXT NOT NULL,
    input_summary  TEXT,                                -- truncated AND redacted; never the raw input
    is_error       INTEGER NOT NULL DEFAULT 0 CHECK (is_error IN (0,1)),
    denied_by      TEXT CHECK (denied_by IN ('hook','permission')),
    deny_reason    TEXT,
    UNIQUE (run_id, seq)
) STRICT;

CREATE INDEX tool_calls_name ON tool_calls (tool_name);
CREATE INDEX tool_calls_denied ON tool_calls (denied_by) WHERE denied_by IS NOT NULL;

CREATE TABLE operator_messages (
    id               INTEGER PRIMARY KEY,
    task_id          TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    message          TEXT NOT NULL,
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    consumed_at      TEXT,
    consumed_by_run  INTEGER REFERENCES runs (id)
) STRICT;

CREATE INDEX operator_messages_unconsumed ON operator_messages (task_id) WHERE consumed_at IS NULL;

-- Reports to the ticket system, delivered with retries.
CREATE TABLE outbox (
    id            INTEGER PRIMARY KEY,
    task_id       TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    source        TEXT NOT NULL,
    ticket_id     TEXT NOT NULL,
    message       TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','failed')),
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT,
    next_attempt_at TEXT,
    created_at    TEXT NOT NULL,
    sent_at       TEXT
) STRICT;

CREATE INDEX outbox_due ON outbox (next_attempt_at) WHERE status = 'pending';

------------------------------------------------------------------------------
-- Evaluation (golden tickets replayed whenever the skill or model changes)
------------------------------------------------------------------------------

CREATE TABLE eval_cases (
    id          TEXT PRIMARY KEY,
    repo        TEXT NOT NULL,
    base_sha    TEXT NOT NULL,                          -- repo state the case starts from
    title       TEXT NOT NULL,
    body        TEXT NOT NULL,
    rubric      TEXT NOT NULL,                          -- what a correct outcome looks like
    created_at  TEXT NOT NULL,
    retired_at  TEXT
) STRICT;

CREATE TABLE eval_results (
    id             INTEGER PRIMARY KEY,
    case_id        TEXT NOT NULL REFERENCES eval_cases (id),
    task_id        TEXT REFERENCES tasks (id),
    repetition     INTEGER NOT NULL,                    -- each case runs at least twice
    model          TEXT,
    skill_version  TEXT,
    score          REAL,                                -- 0..1 against the rubric
    passed         INTEGER CHECK (passed IN (0,1)),
    skill_loaded   INTEGER CHECK (skill_loaded IN (0,1)),
    cost_usd       REAL,
    duration_ms    INTEGER,
    notes          TEXT,
    created_at     TEXT NOT NULL
) STRICT;

------------------------------------------------------------------------------
-- Secrets
--
-- Three kinds:
--   plain      value stored as-is. Chosen deliberately for a local-only DB; the
--              PreToolUse policy denies the agent any access to the DB file.
--   encrypted  AES-256-GCM ciphertext; the key lives outside the database.
--   vault_ref  only a Vault path, resolved by the daemon at run time.
-- Secrets reach MCP servers / the ticket source through bindings and are never
-- placed in the claude process environment, prompts or logs.
------------------------------------------------------------------------------

-- Metadata about encryption keys. The key material itself is NEVER stored here.
CREATE TABLE encryption_keys (
    id           TEXT PRIMARY KEY,                      -- e.g. "k2026-11"
    algorithm    TEXT NOT NULL DEFAULT 'AES-256-GCM' CHECK (algorithm = 'AES-256-GCM'),
    fingerprint  TEXT NOT NULL,                         -- sha256(key) prefix, to detect a wrong key
    created_at   TEXT NOT NULL,
    retired_at   TEXT                                   -- no new encryptions; still decrypts until re-wrapped
) STRICT;

CREATE TABLE secrets (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,                  -- e.g. "argocd/qc/api-token"
    kind         TEXT NOT NULL CHECK (kind IN ('plain','encrypted','vault_ref')),
    -- kind = 'plain'
    value        TEXT,
    -- kind = 'encrypted'
    ciphertext   BLOB,
    nonce        BLOB,                                  -- 12 bytes, unique per encryption
    key_id       TEXT REFERENCES encryption_keys (id),
    -- kind = 'vault_ref'
    vault_mount  TEXT,
    vault_path   TEXT,
    vault_field  TEXT,
    description  TEXT,
    created_by   TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    rotated_at   TEXT,
    expires_at   TEXT,                                  -- daemon warns before, refuses after
    CHECK (
        (kind = 'plain' AND value IS NOT NULL
             AND ciphertext IS NULL AND nonce IS NULL AND key_id IS NULL AND vault_path IS NULL)
     OR (kind = 'encrypted' AND ciphertext IS NOT NULL AND length(nonce) = 12 AND key_id IS NOT NULL
             AND value IS NULL AND vault_path IS NULL)
     OR (kind = 'vault_ref' AND vault_path IS NOT NULL
             AND value IS NULL AND ciphertext IS NULL AND nonce IS NULL AND key_id IS NULL)
    )
) STRICT;

-- Which consumer may receive a secret, and under which environment variable.
-- Consumers are processes the harness launches itself: MCP servers (through
-- the `taskpilot mcp-exec` wrapper), the ticket source, prepare commands.
-- There is deliberately no consumer kind for the claude process.
CREATE TABLE secret_bindings (
    id             INTEGER PRIMARY KEY,
    secret_id      INTEGER NOT NULL REFERENCES secrets (id) ON DELETE CASCADE,
    consumer_kind  TEXT NOT NULL CHECK (consumer_kind IN ('mcp_server','source','prepare')),
    consumer_name  TEXT NOT NULL,                       -- "argocd", "splunk", "hasaki-work", ...
    env_var        TEXT NOT NULL CHECK (env_var GLOB '[A-Z_]*' AND env_var NOT GLOB '*[^A-Z0-9_]*'),
    repo           TEXT NOT NULL DEFAULT '*',           -- '*' = every repo
    created_at     TEXT NOT NULL,
    UNIQUE (consumer_kind, consumer_name, repo, env_var)
) STRICT;

-- Every resolve/create/rotate/delete, successful or not. Never contains values.
CREATE TABLE secret_access_log (
    id          INTEGER PRIMARY KEY,
    secret_id   INTEGER REFERENCES secrets (id) ON DELETE SET NULL,
    secret_name TEXT NOT NULL,                          -- kept after the secret is deleted
    at          TEXT NOT NULL,
    action      TEXT NOT NULL CHECK (action IN ('resolve','create','update','rotate','delete','rewrap')),
    actor       TEXT NOT NULL,                          -- daemon | operator:<name>
    consumer    TEXT,                                   -- "mcp_server:argocd"
    task_id     TEXT,
    run_id      INTEGER,
    ok          INTEGER NOT NULL CHECK (ok IN (0,1)),
    error       TEXT
) STRICT;

CREATE INDEX secret_access_log_secret ON secret_access_log (secret_name, at);

-- Single-use credentials handed to `taskpilot mcp-exec` for one run and one
-- consumer. Only a hash is stored; the wrapper consumes it at startup, so a
-- token seen later in a process listing cannot be replayed.
CREATE TABLE run_tokens (
    token_hash     TEXT PRIMARY KEY,                    -- sha256 of the token
    run_id         INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    consumer_kind  TEXT NOT NULL,
    consumer_name  TEXT NOT NULL,
    expires_at     TEXT NOT NULL,
    consumed_at    TEXT
) STRICT;

------------------------------------------------------------------------------
-- Reporting views
------------------------------------------------------------------------------

-- Approval rate and operator latency per gate (the plan-gate approval rate is
-- the signal for widening automation).
CREATE VIEW v_gate_stats AS
SELECT gate,
       count(*)                                          AS requests,
       count(*) FILTER (WHERE decision = 'approved')     AS approved,
       count(*) FILTER (WHERE decision = 'rejected')     AS rejected,
       count(*) FILTER (WHERE decision IS NULL)          AS pending,
       round(1.0 * count(*) FILTER (WHERE decision = 'approved')
                 / nullif(count(*) FILTER (WHERE decision IS NOT NULL), 0), 3) AS approval_rate,
       round(avg((julianday(decided_at) - julianday(created_at)) * 1440), 1)          AS avg_wait_minutes
FROM approvals
GROUP BY gate;

-- Inbox: everything waiting for the operator, oldest first.
CREATE VIEW v_inbox AS
SELECT t.id AS task_id, t.title, t.repo, t.state, a.id AS request_id, a.gate, a.summary,
       coalesce(a.created_at, t.updated_at) AS waiting_since
FROM tasks t
LEFT JOIN approvals a ON a.task_id = t.id AND a.decision IS NULL
WHERE t.state IN ('awaiting_approval','needs_attention','failed')
  AND t.taken_over_at IS NULL
ORDER BY waiting_since;

-- Denied tool calls per tool and reason: where the agent and the policy disagree.
CREATE VIEW v_denials AS
SELECT tc.tool_name, tc.denied_by, tc.deny_reason, count(*) AS n, max(r.started_at) AS last_seen
FROM tool_calls tc JOIN runs r ON r.id = tc.run_id
WHERE tc.denied_by IS NOT NULL
GROUP BY tc.tool_name, tc.denied_by, tc.deny_reason
ORDER BY n DESC;

-- Cost per repo and day.
CREATE VIEW v_cost_daily AS
SELECT date(r.started_at) AS day, t.repo, count(*) AS runs, round(sum(r.cost_usd), 2) AS cost_usd
FROM runs r JOIN tasks t ON t.id = r.task_id
GROUP BY day, t.repo
ORDER BY day DESC, cost_usd DESC;

PRAGMA user_version = 1;
