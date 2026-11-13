-- taskpilot schema v6: an approval request can carry multiple-choice questions
-- for the operator (MCP tool ask_operator, request_approval.questions, or a
-- Claude Code AskUserQuestion call the hook forwards). JSON array; '' = none.
ALTER TABLE approvals ADD COLUMN questions TEXT NOT NULL DEFAULT '';
