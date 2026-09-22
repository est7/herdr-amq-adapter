# herdr-amq-adapter

A [Herdr](https://herdr.dev) plugin that lets the coding agents running in
Herdr panes message each other over [AMQ](https://github.com/est9/amq)
(agent message queue). It changes neither tool: it wires AMQ's external
injector hook (`amq wake --inject-via`) to Herdr's agent delivery surface
(`herdr agent prompt`).

```
agent A pane ──amq send --to B──▶ AMQ maildir ──▶ amq wake (B's waker)
                                                     │  --inject-via
                                                     ▼
                                    herdr-amq-adapter inject <pane-of-B> <notice>
                                                     │
                                                     ▼
                                    herdr agent prompt <pane-of-B> "AMQ [..]: message from A"
```

## Convention: the Herdr agent name IS the AMQ handle

The adapter only adopts **named** Herdr agents. The live name is used as
`AM_ME` for the waker, so the agent (which knows itself as `AM_ME` from its
own launch) and its waker agree on identity. Naming an agent is the opt-in;
unnamed agents are left alone.

```bash
herdr agent start reviewer --kind claude --pane w1:p2     # named at start
herdr agent rename w1:p3 impl                             # or name later
```

`AM_ROOT` is not copied from the agent process (macOS hides other
processes' environments). The waker runs with `cwd` set to the agent's pane
cwd so `amq` resolves the queue root exactly as the agent's own shell did.
If you pin roots per project with `amq setup`, that is enough; if you rely on
an exported `AM_ROOT`, export it in the environment Herdr's server was
started from.

## Lifecycle

| Herdr event | Adapter action |
|---|---|
| `pane.agent_detected` (not released) | `herdr agent get <pane>`; if named, spawn `amq wake --me <name> --inject-via <self> …` detached (setsid, stdio → `$STATE/logs/<pane>.log`) and record it under `$STATE/wakers/<pane>.json` |
| `pane.agent_detected` with `released: true` | SIGTERM the recorded waker, drop the record |
| `pane.closed`, `pane.exited` | same |
| `[[startup]]` / action `reconcile` | `herdr agent list` vs records: stop stale (pane gone, renamed, unnamed, dead pid), start missing |

Status changes (`idle` / `working` / `blocked`) are deliberately **not**
hooked: AMQ's own retry loop plus the injector's progress markers cover
delivery timing.

## Injector protocol

`amq wake` runs `<self> inject <pane_id> <payload>` per notification and
reads `AMQ_INJECT_PROGRESS=<marker>` from the injector's stderr.

| `herdr agent prompt` result | marker | exit | meaning |
|---|---|---|---|
| exit 0 | `accepted` | 0 | text + Enter written; cohort acknowledged (`--retry-until injected`) |
| `agent_blocked` | `deferred` | 0 | agent is at an approval / question UI; keep the cohort, retry later |
| `agent_not_found`, `agent_prompt_stalled`, timeout, usage | `failed` | 1 | terminal for this unchanged cohort; a new inbox change re-arms |

The injector never passes `--wait`, and its own timeout (4s) stays under
`amq --inject-timeout` (5s) so AMQ always sees a marker rather than a kill.

## Install

```bash
# development: link this checkout (build manually first)
go build -o bin/herdr-amq-adapter ./cmd/herdr-amq-adapter
herdr plugin link ~/EstProjects/herdr-amq-adapter
herdr plugin enable est9.amq-adapter

# inspect
herdr plugin action invoke est9.amq-adapter.status
herdr plugin log list --plugin est9.amq-adapter
```

Requires `amq` on the PATH Herdr's server sees (or `AMQ_BIN`), Herdr ≥ 0.9.0
(plugin event hooks for `pane.agent_detected`).

## Smoke test (two named agents in one project)

```bash
cd ~/some/project && amq init            # once per project
herdr pane split --current --direction right --cwd "$PWD" --no-focus   # -> w1:p2
herdr agent start reviewer --kind claude --pane w1:p2
herdr agent rename --current impl        # name the agent you are driving from
# in the impl agent:
amq send --to reviewer --subject ping --body "reply with pong"
# expected: reviewer's pane receives "AMQ [..]: message from impl" within ~1s
herdr plugin action invoke est9.amq-adapter.status
```

## Layout

- `internal/adapter/` — functional core: `Decide` (event → action),
  `ClassifyPromptResult` (herdr error → AMQ marker), `Plan` (reconcile diff),
  `AmqWakeArgs` (argv contract); plus the shell (`Store`, `Herdr`, `Spawn`).
- `cmd/herdr-amq-adapter/` — the four entry points `hook`, `reconcile`,
  `inject`, `status`.
- `herdr-plugin.toml` — manifest; `[[build]]` compiles the binary on GitHub
  installs.

## Known gaps

- Exit status for `deferred` is not documented by `amq wake --help`; the
  adapter exits 0 with the marker. Verify against the amq source before
  relying on `agent_blocked` retention.
- No `working` gate: a notice is injected even while the agent is mid-turn
  (Claude Code / Codex queue it). Matches the ghostty-bridge behavior orch
  uses today; a `--defer-while-working` option is the obvious next step.
- Windows is excluded: `Setsid` and process-group SIGTERM are Unix-only.
