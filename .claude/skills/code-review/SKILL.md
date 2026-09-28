---
name: code-review
description: Adversarial senior review of a Gombit diff or pull request. Determines whether the change deserves to merge against its claimed contract, the agent working agreement, and locked architecture decisions. Use when the user asks for a code review, PR review, diff review, or to check a change before merge.
---

# Code Review

Review the current diff (or a named PR/branch) as an adversarial senior reviewer. Read `AGENTS.md` first. This is the Claude Code project skill that `AGENTS.md` and `CLAUDE.md` refer to; it overrides the bundled `/code-review` for this repo.

The Cursor counterpart is `.cursor/skills/code-review/SKILL.md`. Keep both aligned, including [references/adversarial-review.md](references/adversarial-review.md).

## When not to use

- Implementing a new backlog item → treat as a feature (Cursor: `create-feature`)
- Reproducing and fixing a defect → treat as a bugfix (Cursor: `bugfix`); review the fix afterward with this skill

## Setup

1. Determine the review surface: `git diff` / `git diff main...HEAD` / the named PR.
2. Identify the linked issue (`[ID] ...`) and its acceptance criteria from the GitHub issue (v0.1 issues also mirror `docs/GOMBIT_BUILD_PLAN.md` §4, a historical record).
3. If the repo is still pre-code, review docs and scaffolding for invented APIs, scope creep, and decision regressions — not imaginary runtime files.
4. Load [references/checklist.md](references/checklist.md). Walk only the sections the diff touches. Treat each item as a contract to attack, not a substitute for tracing the change end-to-end.
5. **Read [references/adversarial-review.md](references/adversarial-review.md) in full before writing any review.** That file is the review: persona, method, severity, output format, and merge standard. Do not fall back to a diplomatic template, a findings dump, or the old Verdict / Blockers / Major / Minor outline.

## How to review

Follow [references/adversarial-review.md](references/adversarial-review.md) exactly.

Technical analysis comes first. Personality is presentation only. Do not invent a problem to produce an entertaining review. Do not re-litigate locked decisions (Huma, feature-packages, Atlas, JWT-in-memory, D10 envelope, AST-only generators, Cobra, ADR-013). If the author did re-litigate one, that is **BLOCKING**.

The review you write must begin with exactly one of `# APPROVE`, `# COMMENT`, or `# REQUEST CHANGES`, then a short opening assessment, numbered findings, and `# VERDICT`. Do not use a different heading scheme.

Suggest the fix; do not implement unless the user asked.
