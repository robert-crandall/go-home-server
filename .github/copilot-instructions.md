# Copilot instructions

Read [`AGENTS.md`](../AGENTS.md) at the repo root first. It is the single source of truth
for setup, build/test commands, layout, conventions, and the gotchas that will otherwise
waste your time. Nothing about them is duplicated here.

## Code review

See gotcha 5 in `AGENTS.md` for the threat model and the list of already-accepted risks.
Check a finding against that list before raising it.

Worth raising: a bug, regression, security issue, or broken behavior in the code a change
actually touches. Reachable failure modes.

Not worth raising: naming and comment wording, hypothetical failure modes that need an
attacker already inside the LAN, pre-existing issues the change didn't introduce, and
"consider also adding…" suggestions. A finding that amounts to more defensive machinery
for an unreachable failure is a downgrade here - simpler-but-correct is the goal.
