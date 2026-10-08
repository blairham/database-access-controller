# database-access-controller

Provisions the role, schemas, grants and object ownership a service needs
inside an RDS, Aurora or self-managed PostgreSQL database.

## Install

```sh
helm install database-access-controller charts/database-access-controller \
  --namespace database-access-controller-system --create-namespace \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::<acct>:role/<role>
```

The annotation is for IRSA; with EKS Pod Identity, associate the role with the
ServiceAccount instead. The IAM role needs `rds-db:connect` on the admin user's
dbuser ARN.

## Values

| Key | Default | Notes |
|---|---|---|
| `replicaCount` | `2` | Leader-elected, so the second is a standby |
| `image.repository` | `ghcr.io/blairham/database-access-controller` | |
| `image.tag` | `""` | Falls back to `appVersion`; pin a digest where it matters |
| `crds.install` | `true` | Installs the `DatabaseAccess` CRD |
| `crds.keep` | `true` | Keeps the CRD on uninstall |
| `rbac.create` | `true` | ClusterRole + bindings |
| `serviceAccount.annotations` | `{}` | e.g. the IRSA role ARN |
| `watchNamespace` | `""` | Empty watches all namespaces |
| `enableLeaderElection` | `true` | |
| `podDisruptionBudget` | `{}` | e.g. `{maxUnavailable: 1}` |
| `configureDefaultAffinity` | `true` | Soft anti-affinity across nodes |
| `metrics.serviceMonitor.enabled` | `false` | Needs the Prometheus operator CRDs |
| `prometheusRule.enabled` | `false` | Alerts on the per-resource metrics; needs the Prometheus operator CRDs |
| `prometheusRule.rules.notReady` | `for: 15m`, `critical` | `database_access_controller_access_ready == 0` |
| `prometheusRule.rules.stale` | `7200s`, `for: 10m`, `warning` | Not planned against the database in two drift intervals (either mode) |
| `prometheusRule.rules.notConverged` | off; `for: 2h`, `warning` | `database_access_controller_access_pending_statements > 0` |
| `autoscaling.enabled` | `false` | Leave off: only the leader reconciles |

### The CRD is in `templates/`, not `crds/`

Helm never upgrades `crds/`, so a schema change would never reach an existing
install. In `templates/` the CRD upgrades with the release; set
`crds.install=false` to manage it separately. `crds.keep` adds
`helm.sh/resource-policy: keep`, because deleting the CRD deletes every
`DatabaseAccess`, and those with `revokeOnDelete` would revoke their grants.

## Generated files

`templates/crds.yaml` and `templates/rbac.yaml` are copied from `config/` by
`hack/sync-chart.sh`. Change the kubebuilder markers in the Go source and run
`make generate`; never edit them by hand.
