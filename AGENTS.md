# AGENTS.md

## Project overview

`devantler-tech/bookkeeper` sends supplier invoices from a mailbox to
[Numina](https://numina.app), a registered Danish bookkeeping system. It is one
Go program with one command, `collect-vouchers`, and a Helm chart that runs it
as a daily Kubernetes CronJob. Every decision is a fixed rule: no AI, no model
tokens. The module path is `github.com/devantler-tech/bookkeeper`, and the Go
version is whatever `go.mod` declares.

The design is deliberately small. Before adding a part — a dependency, a
service, a second command, a config option — name the concrete gain it brings
the owner. Booking, reconciliation and VAT stay in Numina.

## Repository structure

- `main.go` — the CLI: flags, secret files, the HTTP client (no redirects), exit codes.
- `internal/mail` — a JMAP client limited to reading mail and setting one keyword. It refuses any other method and never creates or destroys mail.
- `internal/numina` — uploads a file to Numina's inbox (`POST /v1/attachments`) with OCR.
- `internal/vouchers` — the run: find invoice mail, upload its PDF/PNG/JPEG attachments, mark the mail.
- `charts/bookkeeper` — the Helm chart (CronJob, ServiceAccount, values schema).
- `CLAUDE.md` / `GEMINI.md` — exact one-line `@AGENTS.md` shims; never copy guidance into them.
- `.claude/skills/maintain/SKILL.md` — the maintenance skill card; it defers to *Maintenance* below.
- `.github/workflows/` — `ci.yaml` (required checks), `cd.yaml` (GoReleaser on `v*` tags), `release.yaml`, `template-sync.yaml` (weekly sync of template-owned plumbing from `devantler-tech/go-template`, honouring `.templatesyncignore`), `todos.yaml`, `stale-issues.yaml`.

## Validation

Run before opening a PR:

```bash
go build ./... && go test -race ./...
golangci-lint run
helm lint charts/bookkeeper --values charts/bookkeeper/ci/test-values.yaml
helm template t charts/bookkeeper --values charts/bookkeeper/ci/test-values.yaml > /dev/null
```

Tests use `httptest` servers for JMAP and Numina; they need no network,
mailbox or Numina account.

## Rules

- **No private data in the repository, issues, PRs or logs.** No supplier
  names, amounts, mail subjects, file names or credentials. The program logs
  counts only; keep it that way.
- **Never widen what the program can do to mail or the books.** It reads mail
  and sets one keyword; it uploads to Numina's inbox and nothing else.
- **Stdlib only.** `.golangci.yml`'s `depguard` allows the standard library and
  this module; adding a dependency needs a stated reason.

## Maintenance

The roadmap of record is GitHub Issues. Shared conventions (draft PRs,
Conventional-Commit titles, trust gate, signed commits) come from the
devantler-tech monorepo `AGENTS.md`. Never run an external contributor's
branch locally.

Task menu (≤1 high-value item per run): triage new issues and PRs; keep
dependencies, the Go toolchain and pinned actions current; keep CI green;
maintain your own PRs.
