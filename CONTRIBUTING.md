# Contributing to bookkeeper

Fix a bug, tighten a rule or clarify the docs. Discuss anything larger in an
[issue](https://github.com/devantler-tech/bookkeeper/issues) first: the program
deliberately stays small. Report sensitive vulnerabilities through the private
[Report a vulnerability](https://github.com/devantler-tech/bookkeeper/security/advisories/new)
form, never a public issue or PR.

Never put real invoices, supplier names, amounts or credentials in an issue,
PR, test or fixture.

## Make a change

You need Git, Go meeting [go.mod](go.mod), golangci-lint v2 and Helm 3.
Fork the repository, then:

```bash
git clone https://github.com/YOUR-LOGIN/bookkeeper.git
cd bookkeeper
go build ./... && go test -race ./...
golangci-lint run
helm lint charts/bookkeeper --values charts/bookkeeper/ci/test-values.yaml
```

Commit with a signed [Conventional Commit](https://www.conventionalcommits.org)
message, for example `git commit -S -m "fix(vouchers): skip empty attachments"`,
and open a **draft** PR to `devantler-tech/bookkeeper:main` using the inherited
[PR template](https://github.com/devantler-tech/.github/blob/main/.github/PULL_REQUEST_TEMPLATE.md).
The PR title becomes the release note.
