# config/

Generated manifests. **Nothing here is an install path** — that is
`charts/database-access-controller`.

| Path | Produced by | Consumed by |
|---|---|---|
| `crd/` | `controller-gen crd` | the envtest suite, and `hack/sync-chart.sh` |
| `rbac/role.yaml` | `controller-gen rbac` | `hack/sync-chart.sh` |

Both are rewritten by `make generate`. Editing either by hand is pointless: the
next `make generate` overwrites it, and `make check-generated` fails the build
in the meantime.
