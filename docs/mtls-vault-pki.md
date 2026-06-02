# OpenRails Service mTLS With Vault PKI

OpenRails uses mTLS for standalone service-to-service HTTP. Public/admin/webhook traffic stays on `PORT=2053`; service traffic uses `SERVICE_MTLS_PORT=2054`.

This is a hard cut from the old private-port/API-key model. Do not configure `PRIVATE_PORT`, `OPENRAILS_API_KEY`, `API_KEY`, or `X-API-KEY` for service-route access.

## Local Docker Compose

Local development uses HashiCorp Vault PKI through the `mtls` compose profile:

```sh
docker compose -f docker-compose.yaml --profile mtls run --rm vault-mtls-render
```

The render job writes certificates into the shared Docker volume named `openrails_mtls`:

- `server.crt`, `server.key`, `ca.crt` for the OpenRails mTLS listener
- `clients/authkit.internal/client.crt|client.key|ca.crt`
- `clients/orchestrator.internal/client.crt|client.key|ca.crt`
- `clients/doujins.internal/client.crt|client.key|ca.crt`
- `clients/hentai0.internal/client.crt|client.key|ca.crt`

Host app compose stacks should mount the same volume read-only and call `https://billing:2054` or `https://openrails:2054`, depending on the service DNS name in that stack.

## Kubernetes Shape

Use Vault as the PKI authority and cert-manager as the Kubernetes certificate controller:

1. Enable a Vault PKI mount for OpenRails service identities.
2. Configure a server role for the OpenRails service DNS names.
3. Configure client roles for each caller identity, such as `doujins.internal` and `hentai0.internal`.
4. Create a cert-manager Vault `Issuer` or `ClusterIssuer`.
5. Create one `Certificate` for the OpenRails server Secret and one per caller workload Secret.
6. Mount Secrets into pods as files and configure OpenRails/client env vars to point at those files.

Leaf certificate duration should be 7 days. cert-manager should renew before expiry.

OpenRails reloads its service listener certificate from `service_mtls.cert_file` and `service_mtls.key_file` on new TLS handshakes. Doujins and Hentai0 service clients also reload their client certificate files on new TLS handshakes. This allows Secret-mounted Vault renewals to roll forward without API-key fallback or a coordinated restart, assuming Kubernetes updates the mounted Secret files and existing keep-alive connections drain normally.

The CA bundle is loaded at process start. Rotate the CA as a separate operational event with an overlapping trust bundle and process restart.

## OpenRails Server Config

Required settings when service mTLS is enabled:

```yaml
service_mtls:
  enabled: true
  port: 2054
  cert_file: /run/secrets/mtls/server.crt
  key_file: /run/secrets/mtls/server.key
  client_ca_file: /run/secrets/mtls/ca.crt
  clients:
    doujins.internal:
      scopes: ["entitlements:read"]
    hentai0.internal:
      scopes: ["entitlements:read"]
```

Use `SERVICE_MTLS_CLIENTS` as a JSON object when environment-only configuration is easier.

## Caller Config

Each caller must validate the OpenRails server cert and present its own Vault-issued client cert:

```sh
BILLING_ADMIN_URL=https://billing:2054
BILLING_SERVICE_CERT_FILE=/run/secrets/openrails-mtls/clients/doujins.internal/client.crt
BILLING_SERVICE_KEY_FILE=/run/secrets/openrails-mtls/clients/doujins.internal/client.key
BILLING_SERVICE_CA_FILE=/run/secrets/openrails-mtls/clients/doujins.internal/ca.crt
BILLING_SERVICE_SERVER_NAME=billing
```

The certificate identity must match an entry under `service_mtls.clients`, and that entry must include the scopes required by the route.
