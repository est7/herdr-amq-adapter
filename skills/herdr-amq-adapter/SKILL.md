---
name: herdr-amq-adapter
description: Message other coding agents running in Herdr panes over AMQ. Use when you see "AMQ doorbell … (you are <name> in Herdr; first: source …)", when the user asks you to send, ask, or reply to another agent in Herdr, or when you need to know your own AMQ handle inside Herdr. Requires HERDR_ENV=1.
---

# herdr-amq-adapter

Every agent in a Herdr pane is reachable over AMQ under its Herdr name
(`claude`, `codex`, `claude-2`, or a name the user gave it). The Herdr plugin
`est7.amq-adapter` owns one shared AMQ root for all panes, provisions your
mailbox, and wakes you with a prompt when mail arrives. You launch and
configure nothing.

This is plain AMQ. It is **not** an orch worker context: do not use
`orch worker amq`, `amq coop`, or any `--project` / `--session` routing.
Bare `amq` with the sourced identity is the whole protocol.

## Load your identity first

Your pane has `HERDR_PANE_ID`; the plugin wrote a matching identity file:

```bash
source ~/.config/herdr/plugins/config/est7.amq-adapter/panes/"${HERDR_PANE_ID//:/_}".env
```

That exports `AM_ROOT` and `AM_ME`. Run it in every shell command that calls
`amq` (each tool call is a fresh shell). If the file is missing, the plugin
has not adopted this pane yet: tell the user to run
`herdr plugin action invoke est7.amq-adapter.reconcile` and stop.

## When a doorbell arrives

The doorbell names you and the identity file. Source it, then drain:

```bash
source ~/.config/herdr/plugins/config/est7.amq-adapter/panes/"${HERDR_PANE_ID//:/_}".env && amq drain --include-body
```

Read every message it prints, act on it, then reply in the same thread:

```bash
source … && amq reply --id <message-id> --body "<your answer>"
```

Reply once per message, inline with `--body`; do not write the answer to a
file first. Do not re-drain in a loop; a new doorbell arrives for new mail.

## Find peers and start a conversation

```bash
source … && amq who --json        # handles of agents in Herdr, with liveness
source … && amq send --to codex --subject "<short subject>" --body "<request>"
```

Handles are the names in Herdr's agent sidebar. A send returns immediately;
the peer is woken by the plugin and answers with `amq reply`, which wakes you.
Use `amq thread --id <thread-id>` to review an exchange.

For a multi-round exchange you drive yourself (adversarial review with a
fix loop, single audit, test-hardening attack, or a debate between peers),
read `references/patterns.md`: it holds the role prompts, verdict markers,
and round bounds for each.

## Agents on other machines

Herdr can show saved SSH machines next to Local. Their agents reach you
through the same AMQ mailbox under the name `<machine>-<agent>`
(`heping-codex`), and they see you as `<your machine>-<you>`. Messaging
them is ordinary `amq send --to heping-codex`; nothing else changes.

- The user names a machine ("在 heping 上开个 codex"): resolve it with
  `herdr machine list --json` (match `label` exactly; list the labels and
  ask when nothing matches). Start and inspect agents there with the same
  prefix on every command, `herdr --machine heping pane split …` and
  `herdr --machine heping agent start codex --kind codex --pane <id>`, reading
  ids from that machine's responses; local ids and `--current` do not exist
  there.
- A remote agent needs an explicit working directory. Local defaults to
  the caller's cwd; remote has no such anchor. Use the directory the user
  gave (absolute or `~/`), else the only live workspace on that machine
  (`herdr --machine <label> workspace list`), else ask. Set it with
  `pane split --cwd`; `agent start` inherits the pane's cwd.
- Reply to a `<machine>-<agent>` sender with
  `amq send --to <sender> --thread <thread> --body …`, quoting the thread
  from the message; `amq reply --id` cannot find bridged messages.
- If `herdr --machine` reports the remote does not support machine API
  forwarding, that machine's Herdr is too old; if the agent starts but never
  shows in `amq who`, the plugin is not linked or not paired there.

## Rules

- Never `amq init`, never set `AM_ROOT` or `AM_ME` by hand, never start
  `amq wake`; the plugin owns all three.
- Ignore skills that wrap AMQ for other orchestrators (for example orch's
  `amq-agent`); their worker context does not exist here.
- Delivery waits while Herdr shows you as `working`, retrying with a backoff
  that starts at 5s and caps at 2m between attempts, for as long as the mail
  is pending; finishing your turn is how you receive mail.
