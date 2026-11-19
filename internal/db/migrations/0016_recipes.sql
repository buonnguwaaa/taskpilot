-- taskpilot schema v16: debugging recipes proposed by reflect (taskpilot
-- reflect, Logs tab key R) from the history of tasks. A proposal holds the
-- recipe as YAML until the operator accepts it (written to
-- <home>/recipes/<name>.yaml) or rejects it (kept, so it is not proposed
-- again). reflect_runs records each reflection: its window and cost.
CREATE TABLE recipe_proposals (
    id          INTEGER PRIMARY KEY,
    project     TEXT NOT NULL,
    name        TEXT NOT NULL,
    yaml        TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    evidence    TEXT NOT NULL DEFAULT '',   -- one item per line: task id and what was repeated
    replaces    TEXT NOT NULL DEFAULT '',   -- an existing recipe this one improves
    status      TEXT NOT NULL DEFAULT 'new' CHECK (status IN ('new', 'accepted', 'rejected')),
    created_at  TEXT NOT NULL,
    decided_at  TEXT
) STRICT;

CREATE INDEX recipe_proposals_project ON recipe_proposals (project, status);

CREATE TABLE reflect_runs (
    id         INTEGER PRIMARY KEY,
    project    TEXT NOT NULL,
    at         TEXT NOT NULL,
    since      TEXT NOT NULL,
    tasks      INTEGER NOT NULL,
    proposals  INTEGER NOT NULL,
    cost_usd   REAL NOT NULL DEFAULT 0,
    error      TEXT NOT NULL DEFAULT ''
) STRICT;
