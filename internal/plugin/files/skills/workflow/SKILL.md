---
name: workflow
description: The end-to-end procedure for a taskpilot ticket - orient, plan, get the plan approved, implement, verify, get the PR approved, open PRs into release/qc, wait for QC, open PRs into the base, complete. Use at the start of every taskpilot task and whenever a session is resumed with an operator decision.
---

# taskpilot workflow

You are running headless under taskpilot. Nobody watches this terminal. The
operator only sees what you send through the `taskpilot` MCP tools
(`request_approval`, `complete_task`) and your final message of each turn. Write
those summaries in the operator's language (see `operator_language` in
`task_info`).

Gates are enforced by the harness: a tool call guarded by a gate that is not
approved yet is denied with a message saying which gate. Do not try to work
around a denial with a different command; request the gate instead.

Tools are pre-approved by an allowlist (`allowed_tools` in `task_info`);
anything outside it is denied without asking. A denial covers that one command
only: it does not mean Bash is unavailable, so keep using the allowed commands
(the verify command, `git add`, `git commit`, ...). Change files with Edit and
Write, never with shell redirection (`cat > file`, `sed -i`, `tee`). If a
command you genuinely need is not allowed, say so in your gate request or final
status instead of looking for a workaround.

## Scope: one project, one workspace

The task belongs to one project (e.g. BOM, PLM, payment). The current directory
is the task workspace. It starts empty: open each repo you need with
`open_repo`; it appears as `./<repo>`, a git worktree on the task's work branch,
created from the latest remote base. Only the project's repos can be opened,
and writes are only allowed inside the workspace — never edit the operator's own
checkouts. Run git and verify commands inside the repo directory
(`cd <repo> && ...` or `git -C <repo> ...`).

## 0. On every start or resume

Call `task_info`. It gives the ticket, the project's repos (which are opened,
their verify commands and baselines), the work branch and the approval history,
including the operator's comments on rejected requests. If the session was
resumed with a decision, continue from the matching step.

## 1. Orient (read-only)

- Read the ticket and its acceptance criteria.
- Read the project knowledge files listed in the prompt / `task_info`
  (`context_files`), then decide which repos the change touches (often a
  backend plus a frontend, or a service plus a worker).
- `open_repo` each of them and read the relevant code. Prefer graphify MCP tools
  or `graphify-out/GRAPH_REPORT.md` when available; fall back to Grep/Glob/Read.
- `open_repo` reports the **baseline**: whether the verify command already
  passes on the untouched base. Note pre-existing failures; they are not yours.
- For bugs and production issues, look at the evidence before planning: use
  `splunk_search` (read-only, scoped to the project) to find the errors, the
  affected requests (`request_id`) and when it started. Start narrow
  (`-1h`, a container, `level=error`) and widen only if needed. Quote the
  relevant log lines in the plan. Logs may contain personal or payment data:
  never copy them into commits, PR descriptions or code.
- If the ticket is too ambiguous to plan (no acceptance criteria, contradicting
  requirements, missing reproduction for a bug), go straight to step 2 and make
  the plan request a list of concrete questions instead of a plan.

## 2. Plan -> gate `plan`

Call `request_approval` with `gate: "plan"`:
- `summary`: one or two sentences: what you will change and why.
- `details`: the repos and files to change, the approach, how you will verify
  it, risks and what is out of scope. Mention pre-existing baseline failures.

Then end your turn immediately with a one-line status. Edits are blocked until
this gate is approved.

If the plan is rejected, revise it using the operator's comment and request
`plan` again. Do not start implementing a rejected plan.

## 3. Implement

- Work only in the opened repos, on the work branch.
- Keep the change to what the approved plan says. If you discover the plan is
  wrong (e.g. another repo must change too), request `plan` again with the
  revised plan rather than drifting.
- Commit in small logical commits with Conventional Commit messages, in each
  repo you changed.

## 4. Verify

- Run each changed repo's verify command (from `task_info`) inside that repo.
- If the baseline passed, it must pass now. If the baseline already failed, do
  not fix unrelated failures; compare against the baseline output and make sure
  you introduced no new failure, and run the tests of the packages you touched
  directly (e.g. `go test ./path/to/pkg/...`).
- After 3 failed attempts on the same problem, stop: end your turn explaining
  what fails and what you tried. The operator will see the task as needing
  attention.
- Review your own diff against the base branch and the acceptance criteria
  before asking for review.

## 5. Pull request -> gate `pr`

Call `request_approval` with `gate: "pr"`:
- `summary`: what changed, in which repos, and the verify result.
- `details`: per repo: `git diff --stat` against the base, the commit list, the
  verify output tail (and the baseline comparison when it already failed), and
  anything the reviewer should look at closely.

End your turn immediately.

## 6. PR into QC -> gate `qc`

The team's flow for every repo: the work branch (from the base, usually `main`)
goes into `release/qc` first for testing, and later the SAME work branch goes
into the base. Each repo's `pr_flow` in `task_info` lists the stages.

After `pr` is approved:
- For each changed repo, call `create_pull_request` with that repo, a title
  (Conventional Commit style, ending with the ticket code, e.g.
  `fix(order): show referenced MOs [HSK-9772A8LR]`) and a markdown description
  (what and why, how it was verified incl. the baseline comparison, risks). The
  tool pushes the work branch and opens the PR into the first stage
  (`release/qc`). Do not run `git push` yourself. Never force-push.
- Then call `request_approval` with `gate: "qc"`: `summary` = the QC PR links;
  `details` = a QC test checklist derived from the acceptance criteria (what to
  click/call, expected results, data to prepare) plus a `splunk_search` query
  the tester can use to check the QC logs. End your turn.
- When the session is resumed after the QC PR was merged: call `qc_apps` to
  check the change is actually on QC (the app is Synced/Healthy and its
  revision/image is the merged one), then `qc_logs` (filter by the new code
  path, an ID or `error`) to confirm it runs without errors. Report both in
  the next `qc` request. If the app is OutOfSync or Degraded, say so: syncing is
  the operator's job.
- Never merge `release/qc` (or any other branch) into the work branch: the PR to
  the base must carry only this task's commits. If the QC PR has conflicts,
  report them in the qc request instead of resolving them by merging.

If `qc` is rejected (QC found a bug): read the operator's comment, fix it on
the same work branch (request `plan` again only if the scope changes), verify,
commit, then call `create_pull_request` again for each changed repo — it pushes
the new commits to the existing QC PR — and request `qc` again.

## 7. PR into the base -> done

After `qc` is approved: call `create_pull_request` again for each changed repo;
it opens the PR from the work branch into the next stage (the base, e.g.
`main`). Promoting the base to `release/prod` is a human release step, not
yours. Then call `complete_task` with a summary and all PR links, and end your
turn.

If a PR tool call fails (e.g. a missing branch or Bitbucket error), report the
error in your final status instead of working around it. When the tool says
the operator will open the PR from the TUI (no API token), say so and continue.

Deployments (`deploy` gate: ArgoCD sync and similar) are only in scope when the
ticket asks for them; they follow the same pattern: request, stop, continue on
approval.
