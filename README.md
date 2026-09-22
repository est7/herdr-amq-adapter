# herdr-amq-adapter

A [Herdr](https://herdr.dev) plugin that lets the coding agents in Herdr
panes message each other over [AMQ](https://github.com/avivsinai/agent-message-queue).
Install it, and every agent Herdr detects is reachable by name. No `amq init`,
no environment variables, no manual naming.

```
agent A pane ──amq send --to claude──▶ shared AMQ root ──▶ amq wake (claude's waker)
                                                               │  --inject-via
                                                               ▼
                       herdr-amq-adapter inject <pane> claude <root> <notice>
                                                               │ status gate + prompt
                                                               ▼
                     herdr agent prompt <pane> "AMQ [..]: message from A — you are
                     AMQ agent claude; to read and reply run: source <identity> && amq drain"
```

## Install

```bash
herdr plugin install est7/herdr-amq-adapter        # builds with `go build` on install
# or, for development:
go build -o bin/herdr-amq-adapter ./cmd/herdr-amq-adapter && herdr plugin link "$PWD"
```

Requires Herdr ≥ 0.9.0 and `amq` on the PATH Herdr's server sees (or `AMQ_BIN`).
Then give your agents the companion skill. This repo is a skill source with
the standard `skills/<name>/SKILL.md` layout, so any skills manager that
reads that layout can install it; the manual form is:

```bash
ln -s "$PWD/skills/herdr-amq-adapter" ~/.claude/skills/herdr-amq-adapter   # Claude Code
```

## What it does, zero-config

| moment | plugin action |
|---|---|
| Herdr detects an agent in a pane | if unnamed, `herdr agent rename` it `<kind>` or `<kind>-N` (claude, codex-2 …); register that handle in the shared root; write the pane's identity file; spawn a detached `amq wake` for it |
| mail arrives for that handle | `amq wake` calls `inject`; it reads the agent's live status, and if `idle`/`done`/`unknown` submits the notice with `herdr agent prompt` |
| agent released / pane closed or exited | SIGTERM the waker, remove identity file and record |
| Herdr session restore, or action `reconcile` | diff live agents vs records: retire stale, adopt missing |

Paths (fixed, per user):

- shared root: `~/.local/state/herdr/plugins/est7.amq-adapter/amq-root`
- identity per pane: `~/.config/herdr/plugins/config/est7.amq-adapter/panes/<pane>.env`
  (`export AM_ROOT=… AM_ME=… HERDR_AMQ_PANE=…`)
- waker logs: `~/.local/state/herdr/plugins/est7.amq-adapter/logs/<pane>.log`

Naming is the identity contract: **the Herdr agent name is the AMQ handle**.
Rename an agent in Herdr and its waker is re-spawned under the new handle on
the next reconcile; give an agent a name yourself (`herdr agent start
reviewer …`) and that name is used verbatim.

## Injector protocol

`amq wake` runs `<self> inject <pane> <handle> <root> <payload>` per
notification and reads `AMQ_INJECT_PROGRESS=<marker>` from stderr.

| observed | marker | exit | meaning |
|---|---|---|---|
| status `idle` / `done` / `unknown`, prompt exit 0 | `accepted` | 0 | text + Enter written; cohort acknowledged (`--retry-until injected`) |
| status `working` | `deferred` | 1 | mid-turn; amq retries on its ladder (5s base, 2m cap, no budget spent) |
| status `blocked`, or prompt returns `agent_blocked` | `deferred` | 1 | approval / question UI; same ladder |
| `agent_not_found`, `agent_prompt_stalled`, timeout, usage | `failed` | 1 | terminal for this cohort; a new inbox change re-arms |

`deferred` must exit non-zero: amq's `classifyInjectViaResult` treats a
deferred marker with exit 0 as `uncertain`, which is terminal and never
replayed. The injector never passes `--wait`; its 4s timeout stays under
`amq --inject-timeout` (5s).

## Try it

With two agents open in Herdr (say `claude` and `codex`), in the `claude` pane:

```bash
source ~/.config/herdr/plugins/config/est7.amq-adapter/panes/"${HERDR_PANE_ID//:/_}".env
amq send --to codex --subject ping --body "reply with pong"
```

`codex` receives the notice, drains, replies; `claude` is woken with the
reply. Inspect with `herdr plugin action invoke est7.amq-adapter.status` and
`herdr plugin log list --plugin est7.amq-adapter`.

## Layout

- `internal/adapter/` — pure core: `Decide` (event → action), `ChooseHandle`,
  `AddAgent`, `Notice`, `ClassifyPromptResult` + `GateOnStatus`, `Plan`
  (reconcile diff), `AmqWakeArgs`; shell: `Store`, `Herdr`, `Spawn`, root helpers.
- `cmd/herdr-amq-adapter/` — `hook`, `reconcile`, `inject`, `status`.
- `skills/herdr-amq-adapter/SKILL.md` — the agent-facing prompt contract.

## Known gaps

- Status read and prompt are two calls; an agent that starts a turn in
  between gets the notice queued mid-turn (nothing is lost).
- One shared root per user, not per project; handles are global across
  Herdr workspaces.
- Unix only (`setsid`, process-group SIGTERM).
