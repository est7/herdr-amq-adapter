---
name: herdr-amq-adapter
description: Message other coding agents running in Herdr panes over AMQ. Use when you see a notice like "AMQ [..]: message from <agent> — you are AMQ agent <name>", when the user asks you to send, ask, or reply to another agent in Herdr, or when you need to know your own AMQ handle inside Herdr. Requires HERDR_ENV=1.
---

# herdr-amq-adapter

Every agent running in a Herdr pane is reachable over AMQ under its Herdr
name (`claude`, `codex`, `claude-2`, or a name the user gave it). The Herdr
plugin `est9.amq-adapter` keeps one shared AMQ root for all panes and wakes
you with a prompt when mail arrives. Nothing needs to be launched or
configured by you.

## Load your identity first

Your pane has `HERDR_PANE_ID`; the plugin wrote a matching identity file:

```bash
source ~/.config/herdr/plugins/config/est9.amq-adapter/panes/"${HERDR_PANE_ID//:/_}".env
```

That exports `AM_ROOT` and `AM_ME`. Run it in every shell command that calls
`amq` (each tool call is a fresh shell). If the file is missing, the plugin
has not adopted this pane yet: tell the user to run
`herdr plugin action invoke est9.amq-adapter.reconcile` and stop.

## When a notice arrives

The notice ends with the exact command to run. Run it:

```bash
source ~/.config/herdr/plugins/config/est9.amq-adapter/panes/"${HERDR_PANE_ID//:/_}".env && amq drain
```

Read every message it prints, act on it, then reply in the same thread:

```bash
source … && amq reply <message-id> --body "<your answer>"
```

Reply once per message. Do not re-drain in a loop; a new notice arrives for
new mail.

## Find peers and start a conversation

```bash
source … && amq who --json        # handles of agents in Herdr, with liveness
source … && amq send --to codex --subject "<short subject>" --body "<request>"
```

Handles are the names shown in Herdr's agent sidebar. A send returns
immediately; the peer is woken by the plugin and answers by `amq reply`, which
in turn wakes you. Use `amq thread <thread-id>` to review the exchange.

## Rules

- Never `amq init`, never set `AM_ROOT` yourself, never start `amq wake`; the
  plugin owns all three.
- The `amq-agent` skill, if installed, applies unchanged once the identity is
  sourced.
- Delivery waits while you are `working` in Herdr's view and retries every
  few seconds up to two minutes; finishing your turn is how you receive mail.
