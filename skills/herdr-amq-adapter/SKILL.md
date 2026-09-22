---
name: herdr-amq-adapter
description: Message other coding agents running in Herdr panes over AMQ. Use when you see "AMQ doorbell … (you are <name> in Herdr; first: source …)", when the user asks you to send, ask, or reply to another agent in Herdr (on this machine or a saved SSH machine), or when you need your AMQ identity or delivery status. Requires HERDR_ENV=1.
---

# herdr-amq-adapter

Every agent in a Herdr pane is reachable over AMQ under its Herdr name
(`claude`, `codex`, `claude-2`, or a name the user gave it). The Herdr plugin
`est7.amq-adapter` owns one AMQ root per machine, provisions your mailbox,
and wakes you with a prompt when mail arrives. You launch and configure
nothing.

This is plain AMQ. It is **not** an orch worker context: do not use
`orch worker amq`, `amq coop`, or any `--project` / `--session` routing.
Bare `amq` with the sourced identity is the whole protocol.

## Load your identity first

Your pane has `HERDR_PANE_ID`; the plugin wrote a matching identity file:

```bash
source ~/.config/herdr/plugins/config/est7.amq-adapter/panes/"${HERDR_PANE_ID//:/_}".env
```

That exports `AM_ROOT` and `AM_ME`. Run it in every shell command that calls
`amq` (each tool call is a fresh shell). The doorbell quotes the exact file
path; use it as given. If the file is missing, the plugin has not adopted
this pane yet: tell the user to run
`herdr plugin action invoke est7.amq-adapter.reconcile` and stop.

The registry supports one Herdr server per user. Run reconcile from its
owning session: another socket is refused before lifecycle changes. After
upgrading old records without a socket, the original session must reconcile
first. Report an ownership conflict; keep the existing records intact.

## When a doorbell arrives

Source the identity, then drain:

```bash
source … && amq drain --include-body
```

Read every message it prints and act on it. Do not re-drain in a loop; a
new doorbell arrives for new mail, and a doorbell that finds nothing is
normal (it announced mail you already drained).

## Replying

Reply when the message asks a question, requests work, or the protocol
you are in expects a verdict; a terminal acknowledgment ("thanks", "done")
needs no reply. Reply once per message, inline with `--body`, never via a
file:

```bash
source … && amq reply --id <message-id> --body "<your answer>"
```

AMQ 0.80.1 stores bridged mail under a transfer file name, so `amq reply`
with its header id can answer `message not found`. For a known bridged
message, reply in the same thread instead, quoting the sender and thread
printed by drain:

```bash
source … && amq send --to <sender> --thread <thread> --body "<your answer>"
```

Both forms keep the thread intact; `amq thread --id <thread>` shows it.
For a local message, check the sourced identity and drained id before
treating `message not found` as a cross-machine case.

## Find peers and start a conversation

```bash
source … && amq who --json        # known handles; remote aliases are routes
source … && amq send --to codex --subject "<short subject>" --body "<request>"
```

Handles are the names in Herdr's agent sidebar. Agents on a paired SSH
machine appear under `<machine>-<agent>` (`heping-codex`), and that machine
sees you as `<your machine>-<you>`; messaging them is the same `amq send`.
A send queues the message; the plugin attempts delivery and wakes you when
a reply arrives. A remote alias is a last-known route, not evidence that
the remote agent is online. Paired machines exchange inventories every
30 seconds while the runner and SSH connection are healthy; failed syncs
leave the previous inventory in place. Pairing is configured by the user
with the plugin binary's `peer add` command.

Multiple recipients use `amq send --to a,b`. Remote fan-out preserves the
message id, thread, and refs while each destination takes its turn through
the spool; recipients need not receive it simultaneously.

For a multi-round exchange you drive yourself (adversarial review with a
fix loop, single audit, test-hardening attack, or a debate between peers),
read `references/patterns.md`. To start or inspect an agent on another
machine (`herdr --machine`), read `references/remote-machines.md`.

## Inspect delivery status

Open the status popup (Herdr 0.9.1 or newer):

```bash
herdr plugin action invoke est7.amq-adapter.dashboard
```

It refreshes every five seconds; `r` refreshes, `j`/`k` scroll, and
`q`/Escape closes. It shows local wakers and unread counts, remote routes
and last successful inventory sync, runner freshness, relay reachability,
alias/spool backlog, quarantine, and read errors. An unknown sync time or
an unreadable queue is not proof of a healthy or empty queue.

For command output, run `herdr plugin action invoke est7.amq-adapter.bridge-status`.
For JSON, find `plugin_root` in `herdr plugin list --json` and invoke
`<plugin_root>/bin/herdr-amq-adapter bridge status --json`; inspect `errors`
and the exit code. `status-popup --once` on that binary prints one snapshot.
Neither a successful prompt nor relay acceptance proves that the peer
drained or acted on the message. Full per-message doctor tracing is not
available. A runner version mismatch means rebuilding has not updated the
existing process; report it instead of claiming the new build is active.

## Rules

- Never `amq init`, never set `AM_ROOT` or `AM_ME` by hand, never start
  `amq wake`; the plugin owns all three.
- Ignore skills that wrap AMQ for other orchestrators (for example orch's
  `amq-agent`); their worker context does not exist here.
- Mail arrives while you work: the doorbell is queued into your running
  turn like a user message. Delivery only waits while Herdr shows you as
  `blocked` (an approval or question dialog), retrying with a backoff that
  starts at 5s and caps at 2m between attempts for as long as the mail is
  pending.
