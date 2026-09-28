@AGENTS.md

## Claude Code

- Project skill available: `code-review` (`.claude/skills/code-review/SKILL.md`)
  — adversarial review of a diff/PR against the Agent Working Agreement in
  AGENTS.md (from the historical build plan §5) and the change's claimed
  contract. Invoke with `/code-review` or ask to review a PR/diff; it
  overrides the bundled `/code-review` for this repo.
- For what has shipped — milestones, the Cobra command tree, runtime
  packages — defer to AGENTS.md "Current state", the single place kept
  current; don't restate a snapshot of it here, which is what drifts stale.
  For backlog/dependency lookups, read the GitHub issue (or its epic) —
  `docs/GOMBIT_BUILD_PLAN.md` is the historical v0.1 plan and only covers the
  v0.1 backlog. Generated apps are written by `gombit new`, not committed
  in-tree.
- When creating or editing GitHub issues, keep the `[ID]` title prefix and the
  milestone/label mapping from build plan §6. Don't rename, re-bucket, or
  merge existing issues without being asked.

## Cursor skills

Cursor counterparts live under `.cursor/skills/` (`create-feature`,
`code-review`, `bugfix`). Keep the Claude `code-review` skill and the Cursor
`code-review` skill aligned with this file and AGENTS.md.
