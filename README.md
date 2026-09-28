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
                                                               │ agent prompt
                                                               ▼
                     herdr agent prompt claude "AMQ doorbell run amq drain --include-body then
                     act on it (you are claude in Herdr; first: source <identity>)"
```

## Install and use

Three commands, on every machine that runs Herdr (macOS or Linux, Herdr 0.9.1+).
No Go and no AMQ setup needed.

```bash
# 1. Install. Downloads this machine's prebuilt binary from the release
#    (checked against its SHA256SUMS), and installs amq and amq-bridge into
#    ~/.local/bin when amq is missing.
herdr plugin install est7/herdr-amq-adapter

# 2. Set up, once. Links `herdr-amq-adapter` into ~/.local/bin and the agent
#    skill into ~/.claude/skills (and ~/.codex/skills when Codex is
#    installed), then adopts the agents already open in panes.
herdr plugin action invoke est7.amq-adapter.configure

# 3. Use it. Every agent in a Herdr pane now has a mailbox under its Herdr
#    name. Ask one agent to message another, e.g. in claude's pane:
#      "ask codex to review the current diff over AMQ"
```

The agent's skill does the rest (`amq send`, a doorbell in the other pane,
`amq drain`, `amq reply`). Open the status popup to watch delivery:

```bash
herdr plugin action invoke est7.amq-adapter.dashboard
```

**Update** to the newest release (reinstalls through Herdr, then re-adopts
agents with the new binary):

```bash
herdr-amq-adapter update           # or: update --check to only compare versions
```

**Another machine**: install and configure there the same way, then pair
from this one with `herdr-amq-adapter peer add --ssh <host alias>` (see
[Agents on other machines](#agents-on-other-machines)).

**Skills manager**: this repo also has the standard
`skills/<name>/SKILL.md` layout. `configure` leaves a skill of the same name
that is already there untouched.

Requirements, all installed or checked by step 1:

| what | version | notes |
|---|---|---|
| Herdr | ≥ 0.9.1 | plugin events, `agent prompt`, status popup |
| `amq` | 0.81.1 tested | installed into `~/.local/bin` when missing; an older one gets a warning (upgrade it, then run the reconcile action) |
| `amq-bridge` | same release as `amq` | installed with it; only used across machines |
| `curl` or `wget`, `shasum` or `sha256sum` | any | to download and check the binaries |

Actions (`herdr plugin action invoke est7.amq-adapter.<id>`): `configure`,
`reconcile`, `status`, `bridge-status`, `bridge-ensure`, `dashboard`.
`herdr-amq-adapter version` prints the release version (a source build
prints the VCS revision, with `-modified` when dirty).

### Development

```bash
herdr plugin link "$PWD"            # runs nothing: build first
sh scripts/install.sh               # with Go: builds bin/ from this checkout
herdr plugin action invoke est7.amq-adapter.configure
```

`scripts/install.sh` builds from source in any checkout that is not a
release commit (or with `HERDR_AMQ_ADAPTER_BUILD=source`). Hooks and waker
injections run `bin/herdr-amq-adapter` from the plugin root, so a rebuild
there is live at the next event.

### Releasing

Bump `version` in `herdr-plugin.toml`, commit, tag `v<version>` and publish
the release: `gh release create v<version> --verify-tag --generate-notes`.
`.github/workflows/release.yml` checks the tag against the manifest, runs the
tests, and attaches `herdr-amq-adapter_{darwin,linux}_{arm64,amd64}` and
`SHA256SUMS`, which `scripts/install.sh` downloads.

## What it does, zero-config

| moment | plugin action |
|---|---|
| Herdr detects an agent in a pane | if unnamed, `herdr agent rename` it `<kind>` or `<kind>-N` (claude, codex-2 …); provision its mailbox in the shared root (`amq init --force` with the merged agent list); write the pane's identity file; spawn a detached `amq wake` for it |
| mail arrives for that handle | `amq wake` calls `inject`, which reads the agent's screen and then submits with `herdr agent prompt`; an unsent draft, a trust dialog, or Herdr's `agent_blocked` defers delivery |
| `herdr pane move` gives the pane a new id | re-key the record, write an identity file for the new id, keep the old one (the moved process still sees its original `HERDR_PANE_ID`); the waker is untouched because delivery targets the agent **name**, which Herdr carries across moves |
| agent released, pane stays open | retire the waker; keep the record (pid 0) and identity file so the next agent detected in this pane is offered the same handle (Herdr drops the live name on release) |
| pane closed or exited | retire the waker, remove identity files (current id and aliases) and the record |
| Herdr session restore, or action `reconcile` | diff live agents vs records: retire records whose pane hosts no agent; re-adopt panes whose waker is stale, renamed, still pointing `--inject-via` at a previous plugin build, or still running an AMQ binary older than the installed one (wakers run with `--no-self-upgrade`, so run reconcile after upgrading `amq`) |

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

Mailbox registration has a separate root-wide lock at
`amq-root/meta/.adapter-registry.lock`. Hooks, reconcile, the bridge runner,
and SSH alias registration all hold it across the complete config merge.
Waiting for that lock respects the caller's deadline.

The adapter supports one Herdr server per user-wide registry. Records retain
the owning `HERDR_SOCKET_PATH`; a hook or reconcile from another server is
refused before any lifecycle mutation, including name/pane collisions.
Existing records without this field are bound on the first lifecycle pass;
perform that first reconcile from the original session. This is an ownership
guard, not full multi-session support.

## Injector protocol

`amq wake` runs `<self> inject <pane> <handle> <root> <payload>` per
notification and reads `AMQ_INJECT_PROGRESS=<marker>` from stderr. The
prompt targets `<handle>` (the Herdr live name); `<pane>` only selects the
identity file mentioned in the notice.

Before typing, `inject` runs `herdr agent get` (the agent kind) and
`herdr agent read --source visible --format ansi`, and checks the screen
(`internal/screen`, ported from herdr-projects): `herdr agent prompt`
merges its text with an unsent draft in the input box and submits both,
and Enter on a trust dialog accepts it for every later session in that
folder. Kinds whose input box the check cannot place (anything but claude,
codex, cursor, gemini, opencode, pi) are sent to without it.

| observed | marker | exit | meaning |
|---|---|---|---|
| prompt exit 0 | `accepted` | 0 | text + Enter written; cohort acknowledged (`--retry-until injected`). A working agent queues the notice into its turn |
| input box holds unsent text (`draft_in_box`) | `deferred` | 1 | someone is typing in that pane; retried once the box is empty |
| trust dialog on screen (`trust_screen`) | `deferred` | 1 | waits for the user to answer it in the pane |
| `agent_blocked`, `server_not_running`, screen unreadable, timeout before the prompt | `deferred` | 1 | nothing was typed; amq retries on its ladder (5s base, 2m cap, no budget spent) |
| `agent_not_found`, `agent_prompt_stalled`, timeout during the prompt, usage | `failed` | 1 | terminal for this cohort (a timed-out prompt may have typed); a new inbox change re-arms |

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
  `AgentsWith`, `Notice`, `ClassifyPromptResult`, `Plan`
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
  route inventories follow agent arrivals and departures. Existing mailbox
  data is retained.

Broadcasts preserve the original message id, thread, and refs. AMQ 0.80.1's
spool has one filename per sender/message id, so each destination gets a
successive turn at that slot. The adapter records exact enqueued bytes under
`amq-root/bridge/forwarded/<sender>/<id>__<escaped-destination>.md` before
consuming the alias copy. Earlier `sent/` bytes are preserved in its `archive/`
subdirectory before the slot is reused. Destination-bound transport receipts
recover sends made before these markers existed. Incomplete spool files
(especially a missing `.dest`) are reported and block that sender's push;
they are never sent using an arbitrary default destination.

The rendezvous syncs both the envelope file and its containing directory before
returning `transport_accepted`. A failed persistence step returns an error;
replays repeat the durability step before acknowledging.

Pair from the machine that can SSH to the other. The peer needs Herdr with
this plugin linked (and reconciled once), `amq` and `amq-bridge` installed
at their usual paths; the adapter binary is copied over when the remote
path has none (same OS and architecture assumed; pass `--remote-adapter`
to point at the peer's plugin `bin/`):

```bash
<plugin_root>/bin/herdr-amq-adapter peer add --ssh remote_heping --label heping --me mac \
    --remote-adapter /Users/ada/herdr-amq-adapter/bin/herdr-amq-adapter   # rendezvous served by heping
herdr plugin action invoke est7.amq-adapter.bridge-status
```

`bridge status` shows the runner's last tick, inventory exchange and error,
a live probe of the rendezvous, pending spool and quarantine counts, and
the log path; `--json` gives the same as one object.

Host aliases (`--me`, `--label`/`--host`) are bridge identities: they name
the Ed25519 keys both sides trust and prefix every alias mailbox. Changing
one means pairing again.

Replies: a bridged message is stored under a transfer file name, and amq
0.80 resolves `amq reply --id` by file name, so replying to a
`<host>-<agent>` sender needs `amq send --to <sender> --thread <thread>`
instead. Thread ids survive the hop, so `amq thread --id` shows the whole
exchange on both machines.

## Status popup

Open the read-only dashboard from the Herdr plugin actions, or run:

```bash
herdr plugin action invoke est7.amq-adapter.dashboard
# Equivalent:
herdr plugin pane open --plugin est7.amq-adapter --entrypoint status
```

The popup refreshes every five seconds. `r` refreshes, `j`/`k` scroll, and
`q`/Escape closes it. It shows local waker state, unread messages, the last doorbell per agent
with the reason it was deferred or failed (`<state>/inject/<handle>.jsonl`), remote
route inventory with its last successful sync time, runner freshness, relay
reachability, alias/spool backlog, quarantine, and read errors. Remote inventory
is a last-known route, not proof that a remote agent is online. Older peer files
show an unknown sync time until a new runner successfully exchanges inventory.

`status-popup --once` prints one snapshot for terminal inspection. Spool counts
include only `.md` messages, never `.dest` sidecars. `bridge status --json`
reports unreadable sections through `errors` and exits nonzero; an unavailable
spool inventory is `null`, not an empty map. A per-alias count of `-1` means the
read failed. Full per-message end-to-end doctor tracing is not implemented.

## Departed agents

When an agent's pane closes or its process exits, its handle is reserved for
24 hours (a tombstone under `<state>/tombstones/`): a new unnamed agent of the
same kind gets another name, and the agent can come back under its own. After
that, the next hook or reconcile moves the mailbox, unread mail included, to
`<state>/archive/<handle>-<unix>/` and drops the handle from the amq agent
list; archives older than 30 days are removed. Bridge aliases and handles in
use are never touched.

## Known gaps

- Mail sent to a handle after its mailbox was archived recreates the mailbox
  (`amq send` only warns on unknown handles) and waits there with no waker.
  Mailboxes of agents that left before this version are not collected.

- Herdr's prompt success proves terminal submission, not agent consumption.
  The popup does not claim a drained receipt from a successful injection.
- The screen check reads only the visible screen. For a harness whose input
  box it cannot place, a trust-dialog phrase quoted in visible output defers
  delivery for as long as it stays on screen, which for an idle agent can be
  indefinitely (retried on amq's ladder, not lost; new output clears it).
- One shared root per user, not per project; handles are global across
  Herdr workspaces.
- Unix only (`setsid`).
- Bridge: one rendezvous per federation, served by one host; a peer that
  cannot be dialed from the rendezvous host must dial it. Alias mailbox
  liveness in `amq who` reflects the route, not the remote agent.
- A parked record (agent released, pane open) is forgotten by `reconcile`
  if the pane still hosts no agent at that moment, so the sticky handle does
  not survive a Herdr restart that reconciles before agents are relaunched.
