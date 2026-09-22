# Agents on other machines

Messaging a remote agent needs nothing from this file: it is `amq send
--to <machine>-<agent>`. This file is for starting or inspecting an agent
on a saved SSH machine, which goes through Herdr, not AMQ.

## Resolve the machine

The user names a machine ("在 heping 上开个 codex"). Resolve it with
`herdr machine list --json`, matching `label` exactly; when nothing matches,
list the labels and ask. Then put the same prefix on every Herdr command:

```bash
herdr --machine heping workspace list
herdr --machine heping pane list
herdr --machine heping agent start codex --kind codex --pane <id>
```

Read ids from that machine's responses; local ids, `HERDR_PANE_ID`, and
`--current` do not exist there.

## Choose the working directory

A remote agent needs an explicit directory. Local defaults to the caller's
cwd; remote has no such anchor. Use the directory the user gave (absolute
or `~/`), else the only live workspace on that machine, else ask. The
directory is set when the pane is created; `agent start` inherits it.

## Choose pane or tab

Placement follows the project, on any machine:

- same project directory as an existing pane: split that pane,
  `pane split <id> --cwd <dir> --no-focus`;
- a different project directory: open a new tab in that workspace,
  `tab create --workspace <id> --cwd <dir> --label <project>`, and start the
  agent in its root pane.

## Failure signals

- `herdr --machine` reports the remote does not support machine API
  forwarding: that machine's Herdr is too old.
- The agent starts but never appears in `amq who`: check adoption and
  pairing, then the status popup's runner, last successful inventory sync,
  and errors. Inventory is attempted every 30 seconds; a stopped runner
  or failed SSH exchange leaves old routes in place. An existing alias
  likewise does not prove the remote agent is online.
- The remote agent is `blocked` right after a doorbell: its own CLI is
  asking the user there to approve the `amq` command; only that user can
  answer.
