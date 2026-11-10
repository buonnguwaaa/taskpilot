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

## Attached files

Tickets, operator messages and approval comments may end with a section
`Tệp đính kèm (đã chép, đọc bằng Read):` listing files the operator referenced
(specs, screenshots, exports). They were copied under `~/.taskpilot/refs/` and
are always readable there: open each one with Read before planning (Read
handles text, PDF and images; read a long PDF by page ranges, or give a large
one to `taskpilot:explorer` subagents chapter by chapter). Treat them as part
of the requirement. Files the operator put at the workspace root are meant
the same way.

`.docx` / `.xlsx` are zip archives of XML: read the text with `unzip` piped
into `rg` (only allowed commands may appear in a pipe, so not `sed`), e.g.
`unzip -p spec.docx word/document.xml | rg -o '<w:t[^>]*>[^<]*' -r '$0'` or,
more readable, `unzip -p spec.docx word/document.xml | rg -o '>[^<>]+<'`;
for a sheet: `unzip -p data.xlsx xl/sharedStrings.xml | rg -o '<t[^>]*>[^<]*'`.
Never unzip into a repo. If a file still cannot be read, say so in your gate
request and ask for a PDF or text export instead of guessing its content.

## 0. On every start or resume

Call `task_info`. It gives the ticket, the project's repos (which are opened,
their verify commands and baselines), the work branch and the approval history,
including the operator's comments on rejected requests. If the session was
resumed with a decision, continue from the matching step.

## Production investigation tasks

A ticket whose description starts with `Task type: production investigation`
asks you to find out **why** something fails in production before anything is
changed. The operator created it from the Logs tab: an error group, a search
result, or a symptom someone reported. The evidence it carries (signature,
counts, caller, request_id, sample lines, the SPL) is a starting point, not the
answer.

- Profile: treat it as **Hard** at least (`opus`/`high`) unless the cause is
  obvious from the ticket. The planning run is where the investigation happens,
  so say in your first status line if the default profile is too weak.
- Use the **absolute window** of the ticket with `splunk_search`
  (`earliest`/`latest` in ISO form); relative ranges would have moved since the
  ticket was written. Widen backwards to find when it started.
- Work from evidence to cause:
  1. Timeline: first and last occurrence, rate over time
     (`… | timechart span=10m count`), which services and pods.
  2. Follow one failing request across services (`request_id`, transaction or
     order code) to see where it really breaks; the logging service is often
     only the messenger (e.g. a worker logging a downstream timeout).
  3. Read the code at `caller` / `prefix` and around it; check what changed
     recently in that repo (`git log` on the base branch around the start
     time) — a deploy is a frequent cause.
  4. Check data when state matters: `db_query` on the **QC** database to
     understand the schema and reproduce the situation (never assume QC holds
     the production rows), `qc_logs` to see whether QC shows the same error.
  5. Partner or infrastructure errors (timeouts, 5xx from a provider, DNS,
     connection refused) are causes too: say so, with the evidence, instead of
     forcing a code fix.
- Subagents: when several services or a long time range are involved, run
  `taskpilot:explorer` subagents in parallel (one per service or per question,
  each with the exact window and identifiers) and merge their findings.
- Deliverable: request the `plan` gate with an **investigation report**:
  `summary` = the root cause in one or two sentences (or "not found" and what is
  ruled out); `details` = timeline, root cause with evidence (log lines,
  `path:line`, query results), impact (how many requests / orders /
  transactions, whether customers were affected and since when), the proposed
  fix or operational action with its risk, and the searches that confirm it is
  gone after the fix. Only after the plan is approved, implement it like any
  other task.
- If no code change is needed (partner outage, bad data to correct by hand,
  configuration, an expected error that is only noisy), finish with
  `complete_task` carrying the same report.
- Logs and data contain personal and payment details: quote only what proves
  the point, mask the rest, and never copy them into code, commits or PRs.

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
- When the data matters (a record in a wrong state, a missing row, how a column
  is really filled), use `db_query` on the project's **QC** database
  (read-only MySQL: `SHOW TABLES`, `DESCRIBE t`, then narrow `SELECT … WHERE …
  LIMIT …`). It is QC data, not production: say so when you rely on it. Never
  copy personal or payment data from it into commits, PRs or code. If the
  project has no QC database configured, do without it.
- If the ticket is too ambiguous to plan (no acceptance criteria, contradicting
  requirements, missing reproduction for a bug), go straight to step 2 and make
  the plan request a list of concrete questions instead of a plan.

## 2. Plan -> gate `plan`

First assess the task and call `set_execution_profile` (see "Execution
profile" below). Then call `request_approval` with `gate: "plan"`:
- `summary`: one or two sentences: what you will change and why.
- `details`: the repos and files to change, the approach, how you will verify
  it, risks and what is out of scope. Mention pre-existing baseline failures.
  End with a short "Execution" line: the profile you chose and whether you will
  use subagents for what (e.g. "opus/high; explorer per spec chapter, reviewer
  before the pr gate"), so the operator can correct it when approving.

Then end your turn immediately with a one-line status. Edits are blocked until
this gate is approved.

If the plan is rejected, revise it using the operator's comment and request
`plan` again. Do not start implementing a rejected plan.

## Execution profile (model and effort)

You choose the model and effort the rest of the task runs with. The planning
run uses the operator's default; your choice applies from the next run (the
implementation after the plan gate), and every resume after it. `task_info`
shows `execution_profile`: the current one, who set it, and the allowed models.
If the operator pinned it, keep it and do not argue.

Assess before choosing — how much code and how many repos, how clear the
requirement is, how costly a mistake is, how much reasoning versus typing:

- **Light** — a mechanical or well-located change: copy/config/text, one
  obvious bug, a test to add, following an existing pattern in one place. →
  `sonnet` with `medium` (`low` if trivial).
- **Standard** — a normal feature or bug fix across a few files or one repo,
  clear acceptance criteria, known patterns. → `sonnet`/`high` or
  `opus`/`medium`.
- **Hard** — cross-repo changes, a new service or module, an unclear or long
  spec, concurrency, state machines, money/payment correctness, migrations, a
  production incident with unclear cause. → `opus` with `high` or `xhigh`.
- **Hardest** — novel design with many interacting parts, subtle correctness
  across systems, repeated failed attempts. → the most capable allowed model
  (e.g. `fable`) with `xhigh` or `max`.

Pick from `allowed_models` only. Give a concrete `reason` (what in this task
drove the choice). Re-assess at a later gate only if the task turned out
clearly harder or easier than planned (e.g. drop to `sonnet`/`medium` for a
mechanical QC follow-up, or raise it after verification keeps failing).

## Subagents: when to spawn, when not

You can delegate to subagents with the Agent tool. Use the taskpilot ones:

- `taskpilot:explorer` — read-only research: sweep code, a long spec, logs or
  QC data and return conclusions with evidence. Runs on `sonnet`.
- `taskpilot:reviewer` — independent review of your diff before the `pr` gate.
  Runs on `opus`.
- `taskpilot:implementer` — one self-contained part of the change, in files
  nobody else touches. Uses your model unless you pass another.

Spawn one when it buys accuracy or keeps your context clean:

- The answer needs reading **many files, several repos or a long document**
  (a provider spec, a big module) and you only need the conclusions → one
  `explorer` per area or per spec chapter, **in parallel** (one message, several
  Agent calls). Do this instead of reading 30 files yourself.
- The change is **non-trivial** (Standard or harder, money/state/concurrency,
  more than a handful of files) → a `reviewer` on the finished diff before you
  request the `pr` gate. Give it the ticket, the approved plan and the
  repos/branch — not your opinion of the code — and fix what it finds (or say
  in the gate request why not).
- The work splits into **independent parts with disjoint files** (backend repo
  and frontend repo, separate packages with an agreed interface) → one
  `implementer` per part, in parallel, after you fixed the shared contract
  (types, API, proto) yourself.

Do not spawn when:

- The task is Light, or the files are known and few: do it yourself — a
  subagent costs a full context and loses detail in the hand-off.
- Steps depend on each other in order, or parts would touch the same files:
  stay sequential; never let two agents edit the same file.
- You would just be waiting: do not delegate something and then also do it.
- It is gate or lifecycle work: `request_approval`, `complete_task`,
  `create_pull_request`, `set_execution_profile`, commits and the final verify
  are yours only. Subagents never commit or push.

Brief a subagent like a colleague who knows nothing of the task: the goal, the
exact scope (repos, paths, spec sections), what you already know or ruled out,
and the shape of the answer you need. Pass `model` to override its default when
the part is clearly lighter (`sonnet`) or harder (`opus`) than usual. Treat its
report as input to check, not as truth: verify claims that the plan or the code
depends on. Mention in the gate requests what you delegated.

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
  path, an ID or `error`) to confirm it runs without errors, and `db_query`
  to check the data it wrote on QC when that is part of the change. Report both in
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
