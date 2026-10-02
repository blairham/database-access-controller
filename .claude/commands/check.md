---
description: Build, vet, format-check and test database-controller.
allowed-tools: Bash(go build:*), Bash(go vet:*), Bash(go tool gofumpt:*), Bash(go test:*), Bash(make check-generated)
---

Run each step and report the first failure with its full output. Do not
summarize a failure — paste it.

```sh
go build ./...
go vet -tags 'integration envtest equivalence' ./...
go tool gofumpt -l .   # must print nothing
make check-generated   # generated CRD, RBAC and deepcopy are current
go test -race ./... -count=1
```

Never run golangci-lint by hand, directly or via `pre-commit run --all-files`:
it runs as the pre-commit hook on every commit and in CI.
