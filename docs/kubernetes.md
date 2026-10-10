# Kubernetes

The `openrails` Helm chart runs the standalone server (`openrails run-server`) as
one Deployment. Postgres and Redis are not part of it: bring your own, or run
them beside it as below.

```mermaid
flowchart LR
    I[Ingress + TLS] --> S[Service :80] --> P[openrails pods :3053]
    M[Prometheus] -. /metrics :9090 .-> P
    P --> PG[(Postgres 18+)]
    P --> R[(Redis / Garnet)]
    P -. optional .-> V[Vault]
```

## Install

Each release publishes the chart to `oci://ghcr.io/open-rails/charts/openrails`.
Chart version `X.Y.Z` runs image `docker.io/openrails/openrails:vX.Y.Z`, so the
chart and the server upgrade together. The chart sets `private_port`, so it
needs v0.235.0 or later: pin an older `image.tag` and the server refuses the
key (or set `metrics.enabled: false`).

```bash
helm install openrails oci://ghcr.io/open-rails/charts/openrails --version X.Y.Z \
  --namespace openrails --create-namespace -f values.yaml
```

A minimal `values.yaml`:

```yaml
config:                        # config.yaml (config.example.yaml has every key)
  test_mode: live              # sandbox | live
  provider_write_mode: full    # full | limited | readonly
  public_billing_base_url: https://billing.example.com
  auth:
    issuer: https://billing.example.com
    request_origin: https://billing.example.com
  redis:
    addr: redis:6379
  email_smtp:
    host: smtp.example.com
    port: 587
    username: openrails
    from: "Billing <billing@example.com>"
  trusted_proxies: [10.244.0.0/16]   # your pod CIDR: where the ingress controller runs
  resource_server:
    identifier: https://billing.example.com
    trusted_issuers:
      - name: example
        issuer: https://id.example.com
        merchants: [shop]
        permissions: ["merchant:*"]
files:
  merchants.yaml:              # applied at every boot; secrets go in an overlay
    version: 1
    merchants:
      shop:
        display_name: Shop
secrets:
  env:
    DB_URL: {name: openrails-db-app, key: uri}
  files: [openrails-secrets]   # EMAIL_SMTP_PASSWORD, RESOURCE_SERVER_DPOP_NONCE_KEY
  authKeys: openrails-auth-keys
ingress:
  enabled: true
  className: traefik
  hosts: [billing.example.com]
  tls:
    - secretName: billing-tls
      hosts: [billing.example.com]
```

`config` is rendered into `/etc/openrails/config.yaml` as written. The chart sets
`host`, `port` and `private_port` itself (`ports.http`, `metrics.port`), and
refuses them in `config`. `files` adds more files beside it; the server reads
`merchants.yaml` at every boot, `bootstrap.yaml` (AuthKit's first operators) on
first run, and `catalog.yaml` is the default of `openrails apply-catalog`. A
change to `config` or `files` rolls the pods.

The server refuses to boot on an unknown key, an unset `test_mode` or
`provider_write_mode`, or an `http` issuer. Its log names the key.

## Secrets

The chart creates no Secret and inlines none: every secret comes from a Secret
you already have, in one of four shapes.

| Value | Shape | Use it for |
|---|---|---|
| `secrets.env` | `NAME: {name, key}`: one environment variable from one Secret key | `DB_URL` from CloudNativePG's `uri` |
| `secrets.files` | Secrets whose keys are environment variable names, mounted at `/vault/secrets` | `REDIS_URL`, `REDIS_PASSWORD`, `REDIS_CA_CERT`, `EMAIL_SMTP_PASSWORD`, `VAULT_TOKEN`, `VAULT_SECRET_ID`, `RESOURCE_SERVER_DPOP_NONCE_KEY`, `LLM_API_KEY` |
| `secrets.authKeys` | `keys.json` and `totp.key`, mounted at `/vault/auth` | AuthKit's signing key and its TOTP encryption key |
| `secrets.merchantOverlays` | `{name, key}`: a key holding YAML in the merchant manifest's shape | PSP credentials kept outside Vault |

Any configuration key can come from a secret: its environment variable is its
path upper-cased and joined by `_` (`db.url` is `DB_URL`, `email_smtp.password`
is `EMAIL_SMTP_PASSWORD`). A variable wins over a file of the same name, and both
win over `config`.

Plain Secrets:

```bash
openssl genrsa -traditional -out signing.pem 2048
jq -n --arg pem "$(cat signing.pem)" \
  '{active_key_id: "2026-10", active_private_key_pem: $pem, public_keys: {}}' > keys.json
openssl rand -base64 32 > totp.key        # never rotate: enrolled authenticators depend on it
kubectl -n openrails create secret generic openrails-auth-keys \
  --from-file=keys.json --from-file=totp.key
kubectl -n openrails create secret generic openrails-secrets \
  --from-literal=EMAIL_SMTP_PASSWORD="$SMTP_PASSWORD" \
  --from-literal=RESOURCE_SERVER_DPOP_NONCE_KEY="$(openssl rand -base64 48)"
```

To rotate the signing key, add the new key as active, move the old one to
`public_keys` and update the Secret: the server reloads `keys.json` without a
restart.

**External Secrets Operator.** Name each target key after its environment
variable:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: openrails-secrets
  namespace: openrails
spec:
  refreshInterval: 1h
  secretStoreRef: {kind: ClusterSecretStore, name: vault}
  target: {name: openrails-secrets}
  data:
    - secretKey: EMAIL_SMTP_PASSWORD
      remoteRef: {key: openrails/runtime, property: smtp_password}
    - secretKey: RESOURCE_SERVER_DPOP_NONCE_KEY
      remoteRef: {key: openrails/runtime, property: dpop_nonce_key}
```

**Vault Agent Injector.** Render one file per variable into `/vault/secrets`
with `podAnnotations`, and leave `secrets.files` empty: the injector owns that
directory.

**Vault for merchant configuration** ([vault.md](vault.md)). Declare `config.vault`;
with `auth_method: kubernetes` the chart mounts the pod's ServiceAccount token,
which Vault's kubernetes auth method (mounted at `kubernetes`) exchanges for
the role in `k8s_role`. Bind that role to the ServiceAccount the chart creates
(`<release>-openrails`, or `serviceAccount.name`). With a token instead, put
`VAULT_TOKEN` in `secrets.files`.

## Postgres

OpenRails needs PostgreSQL 18 or later and one role that owns its database: the
server creates its schemas (`billing`, `billing_river`, `profiles`) and migrates
them at boot. [CloudNativePG](https://cloudnative-pg.io) runs that in the
cluster:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: openrails-db
  namespace: openrails
spec:
  instances: 2
  imageName: ghcr.io/cloudnative-pg/postgresql:18
  storage: {size: 20Gi}
  bootstrap:
    initdb: {database: openrails, owner: openrails}
```

It writes the Secret `openrails-db-app`, whose `uri` key is `DB_URL`
(`secrets.env` above). Any other Postgres works the same way: a Secret holding
its URL, with `sslmode=verify-full` across a network.

## Redis

Redis or [Garnet](https://github.com/microsoft/garnet) is optional and has no
default. Without it, OpenRails counts its rate limits, admin lockouts and
captcha challenges in Postgres, shared by every replica; DPoP proofs are always
spent in Postgres. AuthKit's rate limits still need Redis, or
`auth.allow_memory: true`, which keeps them in the process and so allows one
replica only. Nothing in Redis needs a backup.

Name it with `redis.addr` (`REDIS_ADDR`) or a `redis://` or `rediss://` URL
(`REDIS_URL`). `redis.username` (`REDIS_USERNAME`) is an ACL user. TLS comes
from a `rediss://` URL or `redis.tls` (`REDIS_TLS`), and verifies the server
against the system roots or `redis.ca_cert` (`REDIS_CA_CERT`, a PEM bundle). A
declared Redis with invalid settings refuses boot. While it does not answer,
requests count in Postgres, `/metrics` and the error log report it, and
readiness stays green. Credentials and the CA go in `secrets.files`:

```bash
kubectl -n openrails create secret generic openrails-redis \
  --from-literal=REDIS_PASSWORD="$REDIS_PASSWORD" --from-file=REDIS_CA_CERT=ca.crt
```

## Ingress and TLS

TLS ends at the ingress controller (cert-manager, or the controller's own ACME).
Every path goes to the server: the API, `/v1/webhooks/{rail}/{account_id}` for
the PSPs' webhooks, and the admin console when `admin_console.enabled`. Give the
same public origin to `public_billing_base_url`, `auth.issuer`,
`auth.request_origin` and `resource_server.identifier`.

- **Client IP.** Set `config.trusted_proxies` to the CIDR the controller
  connects from (the pod CIDR, or the node CIDR for a host-network controller).
  Without it the server ignores `X-Forwarded-For`, and rate limits, abuse
  tracking and webhook source addresses all see the controller. Behind
  Cloudflare, list its ranges in `cloudflare_proxies` as well.
- **Per-merchant API hosts.** Add a wildcard host (`*.api.example.com`) and a
  wildcard certificate, which needs a DNS-01 challenge.
- **Body size.** Requests are capped at 1 MiB, but a billing archive import
  (`POST /v1/admin/billing-archive`) streams up to 1 GiB: raise the controller's
  limit if you import over HTTP.

## Health and metrics

| Probe | Path | Meaning |
|---|---|---|
| startup, liveness | `/health/live` | The process serves HTTP. It does so only after Postgres answers and migrations finish, so the startup probe allows 10 minutes. |
| readiness | `/health/ready` | Postgres, merchants and the local workers. Redis, Vault and PSPs never fail it. |

On SIGTERM the server drains itself: `/health/ready` answers 503 at once while
it keeps serving for `drain_delay` (`DRAIN_DELAY`, default 5s), so endpoints
drop the pod before it stops accepting; then it finishes in-flight requests
and stops its workers within `shutdown_timeout` (`SHUTDOWN_TIMEOUT`, default
20s). Their sum must fit in `terminationGracePeriodSeconds` (30); raise it with
them. No `preStop` hook is needed.

`metrics.enabled` (the default) serves `/metrics` on the private listener
(`private_port`, 9090): the Service exposes it as port `metrics`, the ingress
never routes it, and `metrics.serviceMonitor.enabled` scrapes it with the
Prometheus Operator.

## Scaling

`replicaCount` is 1. More replicas share the database and need:

- Redis for AuthKit's rate limits (`auth.allow_memory` is one replica only);
  OpenRails' own limits are shared through Postgres either way;
- AuthKit's keys from `secrets.authKeys`: an ephemeral signing key is per pod,
  so a token one pod signs fails on the others. Everything else in `secrets`
  is already the same on every pod.

Every pod runs `run-server`: it serves the API and runs the workers, which
share their queues through Postgres, so a replica adds both. Readiness needs
the local workers, so `run-server --no-workers` never becomes ready, and
`run-worker` has no listener to probe; the chart runs neither.

Then turn on `podDisruptionBudget` and spread the pods with
`topologySpreadConstraints`.

## Upgrades

```bash
helm upgrade openrails oci://ghcr.io/open-rails/charts/openrails --version X.Y.Z \
  --namespace openrails -f values.yaml
```

Rolling upgrades are safe: each new pod migrates at boot under an advisory
lock, so replicas take turns, while the old pods keep serving; schema changes
are additive ([compatibility](compatibility.md)), so the old image runs on the
new schema. A pod whose migration fails never becomes ready, and the rollout
stops there. `helm rollback` returns the previous image; migrations are never
reversed.

## Backups

- **Postgres** is the backup. With CloudNativePG, archive WAL and take base
  backups to object storage (its `backup` section, or the Barman Cloud plugin
  with a `ScheduledBackup`), for point-in-time recovery.
- **The billing archive** is one merchant's portable copy
  ([merchant portability](merchant-portability.md)). It needs every writer
  stopped:

  ```bash
  kubectl -n openrails scale deploy/openrails --replicas=0
  kubectl -n openrails port-forward svc/openrails-db-rw 5432 &
  DB_URL="postgres://openrails:$PASSWORD@127.0.0.1:5432/openrails" \
    openrails billing export --source-stopped --merchant "$MERCHANT_UUID" --out shop.jsonl
  kubectl -n openrails scale deploy/openrails --replicas=1
  ```

- **Keys outside Postgres**: `keys.json`, `totp.key` and what
  [backup and recovery](backup-and-recovery.md) lists. Keep them in your secret
  manager, not only in the cluster's Secrets.

## Operator commands

The image is the CLI. Commands that read the database run in the pod:

```bash
kubectl -n openrails exec deploy/openrails -- \
  openrails --config /etc/openrails/config.yaml apply-catalog --merchant shop
kubectl -n openrails exec deploy/openrails -- \
  openrails --config /etc/openrails/config.yaml intents --merchant shop
```
