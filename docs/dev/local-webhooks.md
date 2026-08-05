# Local webhooks (cloudflared)

Rail sandboxes (e.g. NMI) need a stable public HTTPS URL that forwards to your
local OpenRails. We use a named Cloudflare Tunnel so the hostname is
deterministic — register it once in the provider portal and reuse it across
runs.

Webhook endpoints:

- `/v1/webhooks/{rail}` — merchant derived from payload account identity
- `/v1/webhooks/{rail}/{account_id}` — the receiving PSP account pinned

e.g. `https://<your-hostname>/v1/webhooks/nmi`. `{rail}` is the gateway kind,
never a PSP key — a mobius account still posts to `/v1/webhooks/nmi`.

## Prereqs (one-time, Cloudflare side)

- A Cloudflare account controlling a domain you own
- `cloudflared` installed locally
- A named tunnel + a DNS hostname routed to it (Cloudflare-managed)

## Run

Set in `.env` (or your shell — both scripts source `.env`):

```bash
CLOUDFLARED_TUNNEL_TOKEN=...                       # required (never commit it)
CLOUDFLARED_PUBLIC_HOSTNAME=openrails-webhooks.example.com   # for logging + verify
CLOUDFLARED_TUNNEL_NAME=...                        # optional, logging only
```

```bash
task tunnel-webhooks         # scripts/webhook_tunnel.sh → cloudflared tunnel run --token $CLOUDFLARED_TUNNEL_TOKEN
task verify-webhook-tunnel   # scripts/verify_webhook_tunnel.sh → curls /health/live + /health/ready via the public hostname
```

Ingress (hostname → local port) is configured on the tunnel; see
[cloudflared-config.example.yaml](cloudflared-config.example.yaml) for the
config-file variant (`cloudflared tunnel --config <path> run <tunnel-name>`)
if you prefer that over token mode. The default local compose port is `3053`.

`cloudflared service install <token>` installs a systemd unit that exposes
localhost on boot — avoid it for dev; run the tunnel in a terminal so exposure
is explicit.

## Notes

- One hostname points at one active tunnel target at a time. Multiple
  developers testing simultaneously need distinct hostnames (e.g.
  `openrails-webhooks-alice.<domain>`), each registered with the provider.
- Keep webhook signature verification enabled — seed the merchant's
  `webhook_signing_secret` in merchant config and register the same secret in
  the provider portal; OpenRails rejects unsigned/mis-signed webhooks.
- Never commit tunnel tokens or credentials. Prefer a dedicated sandbox
  hostname; consider Cloudflare Access / IP allowlists.
- Verify end-to-end with the provider portal's "send test webhook" (if
  available) and watch OpenRails logs for signature-verified ingestion.
