# Conversation patterns

Multi-round exchanges you drive yourself over `amq send` / `amq reply`.
There is no runner: **you** are the coordinator. You count rounds, judge
the verdict, and decide when to stop. The role prompts are lifted from
orch's workflow presets; the mechanics are plain AMQ.

## Coordinator duties (every pattern)

- **Brief first.** The peer sees only the mail. Put the task, the tree
  location (cwd, branch, `git diff` range), and the role prompt in the
  body. Nothing in your context reaches them.
- **One thread per exchange.** Open with `amq send`; every later turn is
  `amq reply --id <last message id>` so the thread stays intact and
  `amq thread` shows the whole history.
- **Verdict line.** Ask the peer to start their reply with one of the
  exact markers named in the pattern (for example `verdict: approve`).
  Read the marker, then the findings. A reply without a marker is a
  `reject`.
- **No-progress stop.** Rounds are not capped; the loop ends on
  `approve`, or when a round's findings are the same set as the previous
  round's (same ids, no fix landed), in which case stop and report the
  open findings to the user.
- **Stop when done.** After the final verdict, tell the user the outcome
  and the thread id. Reply to the peer only when the pattern needs
  another round.

## review-loop

Adversarial review of a change with a fix loop. Roles: `implementer`
(usually you), `reviewer` (a peer).

1. Send the reviewer the brief plus the **audit** prompt.
2. Wait for the doorbell. `verdict: approve` ends the loop.
3. On `verdict: reject`, fix every finding by id, or refute it with
   evidence, then reply in the thread with the **fix** report and go
   to 2. If the peer is the implementer instead, send them the **fix**
   prompt and wait for their status reply before re-running the audit.

**audit**

> This review is ADVERSARIAL: assume the change is broken and try to
> prove it; approval you did not try to break is worthless. Audit the
> tree as it exists NOW at the current head: verify against the actual
> diff and by running the tests yourself. The implementer's narration
> is a hypothesis, never evidence. Report findings, do not fix. Each
> finding must be independently actionable and keep a stable id across
> rounds (F1, F2, …). Start your reply with `verdict: approve` or
> `verdict: reject`. approve is a sign-off you personally stand behind;
> reject requires concrete findings with file:line.

**fix**

> Address EVERY finding by id: fix it at root cause, or refute it with
> evidence. Never a shallow patch to quiet the reviewer, never a silent
> skip. Do not expand scope beyond the findings and the original task.
> Report per finding what changed (or why it is refuted), plus tests
> run and results.

## audit

Single adversarial pass, no fix loop. Role: `auditor`. One round.

Send the brief plus the **audit** prompt below. A `reject` terminates:
hand the findings to the user for adjudication instead of fixing them
yourself.

> Single adversarial pass; there is no fix loop after you, a reject
> ends the run for the user to adjudicate. Assume the change is broken
> and try to prove it. Audit the tree as it exists NOW at the current
> head: verify against the actual diff and by running the tests
> yourself; narration is not evidence. Report findings ranked by
> severity, each independently actionable, with file:line. Start your
> reply with `verdict: approve` or `verdict: reject`. approve is a
> sign-off you personally stand behind.

## harden

Attack a change with tests instead of reviewing it. Roles:
`implementer` (usually you), `attacker` (a peer).
Same loop shape as review-loop with these prompts.

**attack**

> Attack the change under test: do not review it, break it. Aim
> targeted tests or mutations at load-bearing behavior. A finding is
> real ONLY when it lands as a concrete failing test at a named path;
> an attack you cannot land as a red test is not a finding. Start your
> reply with `verdict: approve` or `verdict: reject`. reject with the
> landed findings (test path per finding); approve means you attacked
> and the change survived: name what you attacked so the sign-off has
> content.

**fix**

> Make every landed attack green at root cause. Never delete, weaken,
> or skip the attacking test; it stays in the tree as the regression
> net. If an attack is aimed wrong, refute it with evidence instead.
> Report per finding: the fix (or refutation), plus the full suite
> result.

## argue

Debate among equals to converge on a claim set. Roles: `participant`,
two or more (you may be one).

1. Open one thread to all participants (`amq send --to a,b --thread
   <id>`) with the question and the **propose** prompt. Produce your
   own claims before reading any reply if you participate.
2. When every proposal is in, merge the catalogs (keep every id
   unique, never renumber) and reply in the thread with the merged
   catalog plus the **converge** prompt.
3. A round ends when every participant has replied. `verdict: accept`
   from all ends the loop; any `verdict: keep_arguing` starts another
   round with the updated catalog. Two rounds with an identical claim
   set is a no-progress stop.

**propose**

> You are one PARTICIPANT among equals: no judge, no privileged
> synthesizer. Produce your OWN claims independently BEFORE reading any
> peer's proposal. Reply with a claim catalog: one claim per line as
> `C<n>: <statement> — <rationale>`. Keep each claim self-contained and
> falsifiable.

**converge**

> Debate round over the shared claim catalog. Reference claims by their
> existing ids; never renumber. State an explicit stance per claim:
> agree / disagree (with reason) / revise (with replacement text). You
> may add new claims. Start your reply with `verdict: accept` (the
> CURRENT catalog is final) or `verdict: keep_arguing`. False consensus
> is the failure mode: name what you still dispute rather than
> approving to be agreeable.
