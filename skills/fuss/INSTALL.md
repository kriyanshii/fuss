# Using the Fuss skill

Every tool reads the same file, `skills/fuss/SKILL.md`. The files below only
point at it, so there is one copy of the loop to keep up to date.

| Tool | File | How to use it |
|---|---|---|
| Claude Code | `.claude/commands/fuss.md` | Type `/fuss <target and what to test>` in this repo. |
| Cursor | `.cursor/rules/fuss.mdc` | An agent-requested rule: ask the agent to crash-test a program or run Fuss. |
| Codex | `AGENTS.md` | Read automatically in this repo; ask Codex to crash-test a program or run Fuss. |
| Gemini CLI | `GEMINI.md` | Read automatically in this repo; ask Gemini to crash-test a program or run Fuss. |

To use the skill from another repo, copy `skills/fuss/` and the pointer file
for your tool, and keep the paths in the pointer pointing at `SKILL.md`. Fuss
itself still has to be built from this repo (`go build -o fuss .`).
