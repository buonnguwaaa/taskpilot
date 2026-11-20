-- taskpilot schema v18: per-run cost from the session's running total.
-- `claude -p` reports total_cost_usd (and modelUsage) for the whole session
-- so far, resumes included; it used to be stored, and summed, as the run's
-- cost. A run now keeps the session totals it ended with and its cost is the
-- difference from the previous run of the same session.
ALTER TABLE runs ADD COLUMN session_cost_usd REAL NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN session_input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN session_output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN session_cache_read_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN session_cache_write_tokens INTEGER NOT NULL DEFAULT 0;

-- Past runs: their cost_usd is the session total; keep it and store the
-- difference from the highest total of the session's earlier runs (a run
-- with no result has 0, a total lower than an earlier one starts again).
UPDATE runs SET session_cost_usd = cost_usd;
UPDATE runs SET cost_usd = (
    SELECT CASE WHEN p.prev IS NOT NULL AND runs.session_cost_usd >= p.prev
                THEN runs.session_cost_usd - p.prev ELSE runs.session_cost_usd END
    FROM (SELECT id, max(session_cost_usd) OVER (PARTITION BY task_id, session_id ORDER BY id
                 ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS prev FROM runs) p
    WHERE p.id = runs.id);
UPDATE tasks SET cost_usd = (SELECT coalesce(sum(r.cost_usd), 0) FROM runs r WHERE r.task_id = tasks.id);
