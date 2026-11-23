# Standalone example

The course store calling the `openrails` server it runs. Compose starts the
app, PostgreSQL, Redis, Mailpit and the server, built from this checkout; the
app's AuthKit is the server's trusted issuer ([standalone guide](../../docs/standalone.md)).

- `main.go`: the app's AuthKit (it mints the browser's and the backend's tokens
  and pushes users over SCIM) and the remote billing client.
- `openrails/config.yaml`, `openrails/merchant.example.yaml`: the server's
  configuration and its merchant manifest.

```sh
cp openrails/merchant.example.yaml openrails/merchant.yaml   # your NMI sandbox keys
OPENRAILS_CLIENT_SECRET=$(openssl rand -hex 32) docker compose up -d --build
```

The store is http://localhost:8080, the server http://localhost:3053, the inbox
http://localhost:8025. `docker compose down -v` removes it all.
`compose.e2e.yaml` adds a fake NMI gateway and the browser tests.
