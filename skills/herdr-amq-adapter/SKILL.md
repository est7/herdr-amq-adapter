---
name: herdr-amq-adapter
description: Message other coding agents running in Herdr panes over AMQ. Use when you see "AMQ doorbell … (you are <name> in Herdr; first: source …)", when the user asks you to send, ask, or reply to another agent in Herdr (on this machine or a saved SSH machine), or when you need to know your own AMQ handle inside Herdr. Requires HERDR_ENV=1.
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

If `amq reply` answers `message not found`, the mail came from another
machine and is stored under a transfer file name. Reply in the same thread
instead, quoting the sender and thread printed by drain:

```bash
source … && amq send --to <sender> --thread <thread> --body "<your answer>"
```

Both forms keep the thread intact; `amq thread --id <thread>` shows it.

## Find peers and start a conversation

```bash
source … && amq who --json        # every handle you can address, with liveness
source … && amq send --to codex --subject "<short subject>" --body "<request>"
```

Handles are the names in Herdr's agent sidebar. Agents on a paired SSH
machine appear under `<machine>-<agent>` (`heping-codex`), and that machine
sees you as `<your machine>-<you>`; messaging them is the same `amq send`.
A send returns immediately; the peer is woken by the plugin and its answer
wakes you. An agent started on another machine shows up in `amq who` about
30 seconds after that machine's plugin adopts it; the machines must have
been paired by the user (`herdr-amq-adapter peer add`).

For a multi-round exchange you drive yourself (adversarial review with a
fix loop, single audit, test-hardening attack, or a debate between peers),
read `references/patterns.md`. To start or inspect an agent on another
machine (`herdr --machine`), read `references/remote-machines.md`.

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
