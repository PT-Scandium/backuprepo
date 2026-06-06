# AGENTS.md

Guidance for AI coding agents working in this repository. See `CLAUDE.md` for the project spec and `docs/project_notes/` for decisions, key facts, and history.

## Coding Tasks

When spawning Claude Code sessions for coding work, tell the session to use the gstack skills. Start the session prompt with `Load gstack.` followed by the relevant command.

Examples:

- **Security audit:** `Load gstack. Run /cso`
- **Code review:** `Load gstack. Run /review`
- **QA test a URL:** `Load gstack. Run /qa https://...`
- **Build a feature end-to-end:** `Load gstack. Run /autoplan, implement the plan, then run /ship`
- **Plan before building:** `Load gstack. Run /office-hours then /autoplan. Save the plan, don't implement.`
