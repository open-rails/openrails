# Merchant admin console

The React (Vite, shadcn/ui) console OpenRails serves at `admin_console.path`.
How to turn it on, build it and mount it: [docs/admin-console.md](../../docs/admin-console.md).

- `pnpm run dev` serves it against a local OpenRails on `localhost:3053`.
- `bash scripts/build-admin-console.sh` (or `task admin-build`) builds `dist/`,
  which `embed.go` embeds; `dist/` is not committed.
- `src/lib/api/generated/` holds the wire types and route table generated from
  the route catalog (`go run ./scripts/contracts -write`); never edit them.
- Components are shadcn: `pnpm exec shadcn add <name>`.

Browser support for exact money display (Intl decimal strings, BigInt) is in
[docs/money-wire.md](../../docs/money-wire.md#admin-console-browser-support).
