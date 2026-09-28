# CLAUDE.md

Herdr plugin (Go, stdlib only) that gives every agent in a Herdr pane an AMQ
mailbox and types a doorbell into the pane when mail arrives.

- `README.md` is the behaviour contract: lifecycle table, paths, injector
  protocol, bridge, known gaps. Read the relevant section before changing
  behaviour, and update it in the same change.
- `docs/primitives.md` is the layering rule: this plugin owns transport and
  session facts only; anything that judges a message (is it a reply, an
  approval) belongs to orchestrators. Read it before adding a primitive.
- `skills/herdr-amq-adapter/` is what agents in panes read. Keep its commands
  and flags in step with the binary.

## Layout

| path | owns |
|---|---|
| `cmd/herdr-amq-adapter` | CLI dispatch, configure/update, bridge commands, status popup |
| `internal/adapter` | hook/reconcile lifecycle, waker records, `inject`, tombstones + gc |
| `internal/screen` | prompt-box and trust-dialog detection on `herdr agent read` output |
| `internal/bridge`, `internal/rendezvous` | cross-machine federation over `amq-bridge` |
| `internal/durable` | fsync'd file and directory publication |

## Verify

```sh
gofmt -l . && go vet ./... && go test ./...
```

Tests that need real `amq` / `amq-bridge` skip when they are missing. A
skip hides the upstream-contract checks (e.g. the transfer-id drift test in
`internal/bridge/integration_test.go`), so run with both installed before
touching bridge or waker code.

## This checkout is live

Herdr has this directory linked as the plugin root (`source: local`). Hooks
and every waker's `--inject-via` run `bin/herdr-amq-adapter` from here, so
`sh scripts/install.sh` or `go build -o bin/...` replaces the binary that
delivers doorbells to running agents, this session included. Build to a temp
path while iterating; install into `bin/` only a build that passes the
verify line, then run `herdr plugin action invoke est7.amq-adapter.reconcile`
so wakers pick it up.

The installed skill under `~/.claude/skills/` is a vendored copy, not this
repo's `skills/`; editing here does not change what local agents read.

## Invariants

- **inject exit codes**: `deferred` must exit non-zero; amq treats a
  deferred marker with exit 0 as `uncertain`, which is terminal. Keep the
  injector's 4s timeout under amq's 5s `--inject-timeout`.
- **amq owns waker identity**: decide with `amq wake check`, act with
  `repair` / `retire` by exact identity and generation. The plugin never
  reads `ps` and never signals processes.
- **fail closed** on anything the adapter did not create: unexpected
  tombstones, marker files, symlinks (`Lstat`, never write through links),
  incomplete bridge spool entries. gc touches only tombstoned handles.
- **locks**: lifecycle transitions hold the state-dir lock; mailbox
  registration (`amq init --force` merge) holds
  `amq-root/meta/.adapter-registry.lock`; bridge config has its own lock.
  New code that mutates any of these takes the matching lock.
- **durability**: persistence the caller acknowledges goes through
  `internal/durable` (file plus parent directory sync) before returning.

## Upstream contracts

Pinned to Herdr 0.9.1 and AMQ / amq-bridge 0.80.1; comments name the
upstream file each copy comes from. When upstream changes, update the copy
and its test together, and bump the minimum in `scripts/install.sh`
(`AMQ_MIN`), `herdr-plugin.toml`, and the README requirements table.

`internal/screen` and its fixtures are ported from herdr-projects
(`testdata/NOTICE.md`). For identity, trust-dialog, and prompt-box edge
cases, port herdr-projects' verified behaviour rather than inventing a
heuristic, and record limits under README "Known gaps". Fixtures are raw
ANSI (`-text` in `.gitattributes`); capture new ones with
`herdr agent read --source visible --format ansi`.

## Commits and releases

Commit subjects: `type(scope): <gitmoji> summary`, e.g.
`fix(lifecycle): 🐛 fail closed on unexpected tombstones`.

Release: bump `version` in `herdr-plugin.toml`, tag `v<version>`,
`gh release create v<version> --verify-tag --generate-notes`. CI rejects a
tag that differs from the manifest version.
