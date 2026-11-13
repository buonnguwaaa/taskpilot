-- taskpilot schema v11: who assessed a task: the agent (set_execution_profile)
-- or an estimate by Claude from the ticket, shown until the agent assesses.
ALTER TABLE task_assessments ADD COLUMN set_by TEXT NOT NULL DEFAULT 'agent';
