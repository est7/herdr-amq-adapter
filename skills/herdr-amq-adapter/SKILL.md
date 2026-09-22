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
source … && amq reply <message-id> --body "<your answer>"
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
Use `amq thread <thread-id>` to review an exchange.

## Rules

- Never `amq init`, never set `AM_ROOT` or `AM_ME` by hand, never start
  `amq wake`; the plugin owns all three.
- Ignore skills that wrap AMQ for other orchestrators (for example orch's
  `amq-agent`); their worker context does not exist here.
- Delivery waits while Herdr shows you as `working` and retries every few
  seconds up to two minutes; finishing your turn is how you receive mail.
