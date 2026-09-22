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
                     herdr agent prompt claude "AMQ doorbell run amq drain --include-body then
                     act on it (you are claude in Herdr; first: source <identity>)"
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
| Herdr detects an agent in a pane | if unnamed, `herdr agent rename` it `<kind>` or `<kind>-N` (claude, codex-2 …); provision its mailbox in the shared root (`amq init --force` with the merged agent list); write the pane's identity file; spawn a detached `amq wake` for it |
| mail arrives for that handle | `amq wake` calls `inject`; it reads the agent's live status and submits the notice with `herdr agent prompt` unless the agent is `blocked` (a working agent queues it into its turn) |
| `herdr pane move` gives the pane a new id | re-key the record, write an identity file for the new id, keep the old one (the moved process still sees its original `HERDR_PANE_ID`); the waker is untouched because delivery targets the agent **name**, which Herdr carries across moves |
| agent released, pane stays open | retire the waker; keep the record (pid 0) and identity file so the next agent detected in this pane is offered the same handle (Herdr drops the live name on release) |
| pane closed or exited | retire the waker, remove identity files (current id and aliases) and the record |
| Herdr session restore, or action `reconcile` | diff live agents vs records: retire records whose pane hosts no agent; re-adopt panes whose waker is stale, renamed, or still pointing `--inject-via` at a previous plugin build |

Paths (fixed, per user):

- shared root: `~/.local/state/herdr/plugins/est7.amq-adapter/amq-root`
- identity per pane: `~/.config/herdr/plugins/config/est7.amq-adapter/panes/<pane>.env`
  (`export AM_ROOT=… AM_ME=… HERDR_AMQ_PANE=…`)
- waker logs: `~/.local/state/herdr/plugins/est7.amq-adapter/logs/<pane>.log`

Naming is the identity contract: **the Herdr agent name is the AMQ handle**.
Rename an agent in Herdr and its waker is re-spawned under the new handle on
the next reconcile; give an agent a name yourself (`herdr agent start
reviewer …`) and that name is used verbatim. An agent that restarts in the
same pane gets its previous handle back unless another live agent has taken
it, so two `claude` panes cannot swap names across restarts.

Waker identity and liveness belong to amq, not to this plugin: `amq wake
check` says whether the lock holder is live, stale, or missing and names its
generation and saved injector target; the plugin compares that target with
the one it wants for the pane (binary, pane id, handle, root) and then either
keeps it, has amq `repair` a stale one from its saved target, or `retire`s it
by exact identity and generation before starting a replacement. A start is
only recorded once `check` reports it live, so a start that lost the lock
race is never recorded. The plugin never reads pids from `ps` and never
signals processes. Lifecycle transitions (hooks, reconcile) are serialised
by a lock in the state dir.

## Injector protocol

`amq wake` runs `<self> inject <pane> <handle> <root> <payload>` per
notification and reads `AMQ_INJECT_PROGRESS=<marker>` from stderr. The
prompt targets `<handle>` (the Herdr live name); `<pane>` only selects the
identity file mentioned in the notice.

| observed | marker | exit | meaning |
|---|---|---|---|
| status `idle` / `done` / `working` / `unknown`, prompt exit 0 | `accepted` | 0 | text + Enter written; cohort acknowledged (`--retry-until injected`). A working agent queues the notice into its turn |
| status `blocked`, or prompt returns `agent_blocked` | `deferred` | 1 | approval / question UI; amq retries on its ladder (5s base, 2m cap, no budget spent) |
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

## Agents on other machines

Every machine keeps its own root and its own agents; there is no shared or
remote root. Cross-machine mail rides on [amq-bridge](https://github.com/avivsinai/agent-message-queue/tree/main/cmd/amq-bridge),
AMQ's signed, deduplicated courier, and this plugin only provisions and
drives it:

- a remote agent is a local **alias mailbox** `<host>-<agent>` (`heping-codex`);
  `amq send --to heping-codex` is all an agent does;
- `bridge run` (started by every reconcile once paired) re-addresses mail in
  alias mailboxes (`from` becomes the alias the peer knows the sender by,
  `mac-claude-2`), hands it to `amq-bridge enqueue`, pushes and polls a
  **rendezvous** (a loopback blob store this plugin serves on one host and
  the other reaches through an SSH tunnel), and applies inbound envelopes
  into the real agent's inbox, where the ordinary waker rings the doorbell;
- the dialing side syncs agent inventories both ways over SSH every 30s, so
  alias mailboxes appear and disappear with the agents.

Pair from the machine that can SSH to the other (the peer needs Herdr with
this plugin linked, `amq` and `amq-bridge` installed; the adapter binary is
copied over when missing):

```bash
herdr-amq-adapter peer add --ssh remote_heping --label heping --me mac   # rendezvous served by heping
herdr-amq-adapter bridge status
```

Host aliases (`--me`, `--label`/`--host`) are bridge identities: they name
the Ed25519 keys both sides trust and prefix every alias mailbox. Changing
one means pairing again.

Replies: a bridged message is stored under a transfer file name, and amq
0.80 resolves `amq reply --id` by file name, so replying to a
`<host>-<agent>` sender needs `amq send --to <sender> --thread <thread>`
instead. Thread ids survive the hop, so `amq thread --id` shows the whole
exchange on both machines.

## Known gaps

- Status read and prompt are two calls; an agent that opens an approval
  dialog in between gets the notice as a keystroke into that dialog (Herdr
  refuses the prompt when it already sees `blocked`, so the window is the
  gap between the two calls).
- One shared root per user, not per project; handles are global across
  Herdr workspaces.
- Unix only (`setsid`).
- Bridge: one rendezvous per federation, served by one host; a peer that
  cannot be dialed from the rendezvous host must dial it. Alias mailbox
  liveness in `amq who` reflects the route, not the remote agent.
- A parked record (agent released, pane open) is forgotten by `reconcile`
  if the pane still hosts no agent at that moment, so the sticky handle does
  not survive a Herdr restart that reconciles before agents are relaunched.
