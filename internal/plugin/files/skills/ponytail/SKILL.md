---
name: ponytail
description: >
  How to write the code of a taskpilot task: the smallest change that fully
  solves the ticket, reusing what the repo already has. Read it before the
  first edit of every implementation, and use its lean checklist when you
  review your own diff before the pr gate.
license: MIT (adapted from DietrichGebert/ponytail, https://github.com/DietrichGebert/ponytail)
---

# Ponytail: the smallest complete change

Write like a lazy senior developer: the best code is the code never written.
Solve the whole ticket with the least new code. Lazy about the solution, never
about the change: every part the ticket needs is done.

## Before you write

Read the ticket, the approved plan and the code they touch. List every place
the change must reach: callers, tests, fixtures, config, exports, migrations.
Check what the change could break: data it would destroy or expose, callers
that stop working. That is the scope. Extra features are not.

## Take the first option that fully works

1. Does it need to exist? Skip features, options and flexibility the ticket
   did not ask for, and name them in one line in the `pr` request.
2. Already in this repo (a helper, service, repository, component, pattern)?
   Use it the way the surrounding code does.
3. The standard library or a platform/framework feature? Use it, unless the
   repo has its own. A house component beats a native one.
4. A dependency already in go.mod / package.json? Use it. Never add a
   dependency for a few lines.
5. Can it be one line a reader gets at a glance? One line.
6. Otherwise: the minimum code that works.

## Rules

- No abstraction, interface, wrapper, type conversion, option, config,
  boilerplate or "for later" code nobody asked for. An interface with one
  implementation, a config key nobody sets, a helper called once: don't.
- Keep values in the form the code already has. Keep the structure the repo
  already has: its layers, interfaces and conventions. Deletion beats addition.
- The shortest working diff wins, once you know everything it must touch. A
  one-liner that needs decoding is not short.
- Comment only the why the code cannot show, in one line.
- Bug fix: before editing, grep every caller of the function you touch, then
  fix the root cause once in the shared code, not in one caller.
- Code you move or merge keeps its error handling and validation.
- Between options of equal size, take the one correct on edge cases.
- New non-trivial logic (a branch, a loop, a parser, money, security, a data
  write, a bug fix) is checked with one small test that fails when it breaks.
  The operator's rule: tests are for verifying locally, NOT for the PR. Keep
  new test files untracked (never commit them); commit with explicit paths,
  not `git add .` / `-A`. Only fix existing tests the change breaks.
  create_pull_request refuses commits that add test files.
- A shortcut with a known limit gets a comment:
  `shortcut: <the limit>, <when to upgrade>`.

Never cut: validation at trust boundaries, error handling that prevents data
loss, transactions, security, idempotency of payment or stock operations,
anything the ticket or the operator asked for.

## Lean check of your own diff (before the `pr` gate)

Go through `git diff <base>...HEAD` of each repo and remove:
- delete: dead code, unused options, debug output, speculative features
- reuse: logic the repo already has (name the path you reused instead)
- stdlib: hand-written code the standard library or framework already does
- yagni: abstraction with one implementation, config nobody sets
- merge: near-copies that must change together

In the `pr` request details, add one line: what you deliberately skipped or did
not check, and any risk the reviewer must know.
