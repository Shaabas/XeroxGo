---
name: implement-task
description: Execute one approved, narrowly scoped XeroxGo implementation task with minimal changes, targeted tests, and a verifiable handoff.
---

# Implement a XeroxGo task

1. Read root `AGENTS.md` and the assigned entry in `docs/TASKS.md`. Confirm allowed files, branch, acceptance criteria, and prohibited operations.
2. Run `git status --short --branch` and `git log -1 --oneline`. If changes exist, identify their owner; preserve them before proceeding. Never discard local work.
3. Inspect only relevant code/tests. Implement the smallest complete change; preserve API contracts, billing, recovery and UI conventions.
4. If a new architecture, database, payment or deployment decision is required, stop and request approval. Do not guess.
5. Run targeted tests first, then wider tests only when relevant. Record commands and exit results.
6. Inspect `git diff --check` and the actual diff. Commit only task files on the assigned branch.
7. Return: task ID, branch, commit SHA, files, tests, residual risks, next handoff. Propose a concise project-state update if those docs are outside your assigned scope.

Never merge or deploy unless separately and explicitly approved.
