# Cloudflared: deterministic webhook URL for Mobius/NMI sandbox

Mobius/NMI sandbox testing needs a stable public webhook URL that forwards to a local OpenRails instance.

This runbook uses **Cloudflare Tunnel** (`cloudflared`) to map a fixed hostname to `http://localhost:<openrails-port>`.

## Target URL shape

Pick a stable hostname you can register once in the Mobius/NMI portal, for example:

- `https://openrails-webhooks-sandbox.<your-domain>/v1/webhooks/mobius`

## Prereqs

- A Cloudflare account that controls the chosen domain
- `cloudflared` installed locally
- A tunnel created in Cloudflare (named tunnel recommended)
- A DNS hostname pointing at that tunnel (Cloudflare-managed)

## Minimal local workflow

1) Run OpenRails locally (example port `2053`):

- Ensure webhook signature verification is configured (Mobius/NMI): `PROCESSORS_MOBIUS_WEBHOOK_SECRET=...`

2) Run cloudflared with a named tunnel (choose one approach):

- **Option A (token-based):** `cloudflared tunnel run --token <TUNNEL_TOKEN>`
- **Option B (config file):** `cloudflared tunnel --config <path> run <tunnel-name>`

Repo shortcuts (optional):
- `task tunnel-webhooks` (runs `scripts/webhook_tunnel.sh`)
- `task verify-webhook-tunnel` (curls `/health/live` + `/health/ready` via the public hostname)
- `task e2e-mobius-sandbox` (runs compose + tunnel + signed test webhook)

### What is `cloudflared service install ...`?

`cloudflared service install <TOKEN>` installs cloudflared as a **system service** (usually a systemd unit) so the
tunnel starts automatically in the background on boot.

- The “numbers”/long string is a **tunnel run token** that authorizes your machine to connect to that tunnel.
- This is convenient for a single developer machine, but it is not required.

For most dev workflows, prefer running cloudflared in a terminal (Option A/B above) so it’s explicit when you’re
exposing localhost.

To manage the service after installation:
- Check status: `sudo systemctl status cloudflared`
- Stop: `sudo systemctl stop cloudflared`
- Start: `sudo systemctl start cloudflared`
- Disable on boot: `sudo systemctl disable cloudflared`

To remove it, use your OS package/service management (commonly: `sudo systemctl disable --now cloudflared` and remove
the unit file if needed).

### Multi-dev / multi-env note (important)

A single stable hostname can only point to **one active tunnel target at a time**. If multiple developers need to test
webhooks simultaneously, give each developer or environment a distinct hostname, e.g.:

- `openrails-webhooks-sandbox-alice.<domain>`
- `openrails-webhooks-sandbox-bob.<domain>`

and register the corresponding URL(s) in Mobius/NMI (or swap the configured webhook endpoint when needed).

3) Configure ingress to route the hostname to OpenRails.

See `docs/cloudflared-config.example.yaml` for a minimal config template.

Example ingress snippet:

- `hostname: openrails-webhooks-sandbox.<your-domain>`
- `service: http://localhost:2053`

Then Mobius/NMI will call:
- `https://openrails-webhooks-sandbox.<your-domain>/v1/webhooks/mobius`

and cloudflared will forward to:
- `http://localhost:2053/v1/webhooks/mobius`

## Security notes (sandbox still matters)

- Never commit tunnel tokens/credentials.
- Prefer a dedicated sandbox hostname.
- Consider Cloudflare Access / IP allowlists if possible.
- Keep webhook signature verification enabled (OpenRails will reject unsigned Mobius/NMI webhooks).

## Quick verification

- Hit the webhook endpoint through the public hostname and ensure you see requests in your OpenRails logs.
- Use Mobius/NMI portal “send test webhook” (if available) and confirm you receive it locally.
