# Transport primitives

What this plugin owns, what it leaves to layers above, and the gaps between
the two. Status: draft for review (2026-09-27).

## Layering

| layer | concerns | owner |
|---|---|---|
| T transport | addressing, envelope, send/reply/drain, wake and inject, receipts, DLQ, delivery-state facts | AMQ + this plugin |
| S session | identity, handles, adoption and retirement of panes, restart recovery | this plugin (Herdr panes only) |
| P protocol | dispatch-to-reply correlation, reply grammar, verdict markers, report shapes | orchestrators; conventions in `skills/…/references/patterns.md` |
| W workflow | runs, loops, gates, plans, roles, topology, task lists | orchestrators (orch, herdr-projects) |

Rule: the plugin adds only neutral facts and mechanisms. Anything that
classifies or judges a message (is this a reply to my dispatch, is it an
approval) is derived by the caller from its own records, so a peer cannot
mislabel it. orch does exactly this, and herdr-projects keeps reports,
`## Next` and its inbox in its own layer.

## Primitives

| # | primitive | definition | status |
|---|---|---|---|
| T1 | handle | an agent's AMQ mailbox name = its Herdr live name, one shared root per user | have |
| T2 | envelope | AMQ message with `id`, `thread`, `refs`, `kind`, subject, body | AMQ-owned |
| T3 | doorbell | one line typed into the agent: AMQ's notice plus the identity path; never the message body | have |
| T4 | delivery gate | type only when the screen allows it: no unsent draft, no trust dialog, not `blocked`; failures that typed nothing defer | have (`internal/screen`, `Gate`, `ClassifyHerdrResult`) |
| T5 | delivery-state facts per message id | enqueued → doorbell accepted / deferred / retried / invalid → drained (receipt) → DLQ; "injected" never counts as "drained" | have, AMQ-owned: `notification-attempts.jsonl` per agent, `amq trace <id>`, `amq doctor --ops --json` (unread, DLQ, attempts). It records a deferral only as "exit status 1" |
| T6 | delivery reason | why the injector deferred or failed (draft_in_box, trust_screen, server_not_running, …) | have: `<state>/inject/<handle>.jsonl` (bounded, rotated), shown per agent in the status popup. An upstream `AMQ_INJECT_DETAIL` was judged unlikely to land |
| T7 | same-thread redeliver | resend the same body on the same thread with a fresh id so the wake fires again (orch's nudge for undrained mail) | gap; `amq send --thread` already allows it by hand |
| T8 | cross-machine reply | `amq reply --id` answers a bridged message on its thread | gap: AMQ 0.80.1 stores bridged mail under a transfer file name; agents fall back to `send --thread`. Upstream fix |
| S1 | identity binding | the pane → (handle, root) mapping an agent sources | have: identity file keyed by pane id |
| S2 | occupant check | a record is inherited only by an agent with the same cwd and kind (pane ids restart after a Herdr server restart); an earlier occupant's handle is never given to a newcomer | have (`SameOccupant`, `Inherit`, `AdoptHandle`) |
| S3 | verified whoami | resolve the calling pane, check its live agent name equals the recorded handle, print the identity; refuse on mismatch | gap; orch ADR 0007 rejects identity taken from ambient files or env alone |

## Open question: a departed agent's mailbox

When an agent leaves, `stop` retires its waker and record but its mailbox
stays in the shared root. A later unnamed agent of the same kind can be
given that handle by kind-based naming and receive the backlog (pre-dates
S2; S2 only stops the reused-pane-id path). Deciding needs a handle
lifecycle rule: bounce to DLQ, archive, reserve the handle, or accept reuse
as today.

## Proposed order

1. Departed mailboxes (open question above): tombstone + grace period +
   periodic gc.
2. S3: `herdr-amq-adapter whoami` from inside a pane, used by the skill
   instead of sourcing the file blind.
3. T8: report upstream with a reproducer; keep the skill's fallback until
   a fixed AMQ release.
4. T7 only if a real consumer needs it; orchestrators can already resend on
   a thread.

## Out of scope

Dispatch journals, pending-reply tracking, dedup and idempotency keys,
verdict grammars, roles, peer topology, charters, runs, task lists and any
settings UI. These belong to orchestrators built on AMQ.

## Sources

- herdr-projects 0.2.30 (`src/prompt_box.rs`, `src/trust_screen.rs`,
  `src/thread.rs` `agent_matches`, `docs/herdr-notes.md`), read directly.
- orch v0.10.0 (`docs/adr/0007-…`, `internal/team/journal.go`,
  `internal/team/durable_send.go`, `cmd/orch/worker.go`), summarized by a
  reading agent; the file references were not re-checked by hand.
- Live checks on Herdr 0.9.1: `agent prompt` merges an unsent draft and
  submits it; a working Claude queues a prompt and shows a dim hint in its
  box; `server_not_running` and `agent_not_found` error codes.
