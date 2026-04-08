# NMI Tokenization Test Harness (Billing)

Billing itself never runs Collect.js. It only consumes the resulting `payment_token`.

This repo includes a **dev-only** harness page that helps you:
- load Collect.js from a configurable script URL,
- generate a `payment_token` in the browser,
- optionally call billing to create a stored payment method with that token.

## Enable the harness

The debug routes are registered **only when `env=dev`**.

Set (example):
- `ENV=dev`
- `PROCESSORS_<PROVIDER>_TOKENIZATION_KEY=...` (public)
- `PROCESSORS_<PROVIDER>_TOKENIZATION_URL=...` (Collect.js script URL)

Then start the billing server normally.

## Open the harness

- Real mode: `GET /debug/nmi/tokenization?mode=real&provider=<provider>`
- Stub mode (no external calls): `GET /debug/nmi/tokenization?mode=stub&provider=<provider>`

Tip: if your NMI tokenization key enforces an allowed-origin list, load this page via your Cloudflared hostname
(not `http://localhost:2053`) so the browser origin matches what you registered.

Stub mode loads `/debug/nmi/collect-stub.js` and generates a fake token like `tok_stub_<timestamp>`.

## Using real Collect.js

Real mode loads Collect.js from `processors.<provider>.tokenization_url` (or `PROCESSORS_<PROVIDER>_TOKENIZATION_URL`).

If tokenization fails, common causes:
- The tokenization key is not valid for the chosen Collect.js host.
- The tokenization key requires an **allowed origin** and the current hostname is not permitted.
- Mixed-content / HTTPS issues (use HTTPS when required; see the Cloudflared runbook).

## Optional: call billing with the token

The harness can call `POST /v1/me/payment-methods` to create a stored payment method.
This requires:
- A running billing stack with auth configured, and
- A valid user JWT pasted into the harness page.

Note: `POST /v1/me/payment-methods` requires billing address fields; the harness provides defaults you can edit.
