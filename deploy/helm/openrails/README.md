# OpenRails Helm chart

Runs the standalone OpenRails server (`openrails run-server`). Postgres 18+ and
Redis are yours: the chart takes their addresses in `config` and their
credentials from existing Secrets. Walkthrough: [docs/kubernetes.md](../../../docs/kubernetes.md).

```bash
helm install openrails oci://ghcr.io/open-rails/charts/openrails --version X.Y.Z \
  --namespace openrails --create-namespace -f values.yaml
```

Chart version `X.Y.Z` runs image `vX.Y.Z`.

| Value | Meaning |
|---|---|
| `config` | `config.yaml`, rendered as is (keys: `config.example.yaml`). No secrets. |
| `files` | More files in `/etc/openrails`: `merchants.yaml`, `bootstrap.yaml`, `catalog.yaml`. |
| `secrets.env` | Environment variables from Secret keys, e.g. `DB_URL: {name: db-app, key: uri}`. |
| `secrets.files` | Secrets whose keys are environment variable names, mounted at `/vault/secrets`. |
| `secrets.authKeys` | Secret with AuthKit's `keys.json` and `totp.key`, mounted at `/vault/auth`. |
| `secrets.merchantOverlays` | Secret keys holding merchant manifest overlays (PSP credentials outside Vault). |
| `metrics` | The private `/metrics` listener (`private_port`), its Service port and an optional ServiceMonitor. |
| `ingress` | Hosts routed whole to the server; TLS. |
| `replicaCount` | 1. More need Redis for AuthKit's rate limits and DPoP proofs, and `secrets.authKeys`. |
| `podDisruptionBudget`, `topologySpreadConstraints`, `affinity` | Scheduling. |

The chart owns `host`, `port` and `private_port` (`ports.http`, `metrics.port`).
Pods run as UID 1001 with a read-only root filesystem, no service links and no
ServiceAccount token unless Vault's kubernetes auth needs it.
