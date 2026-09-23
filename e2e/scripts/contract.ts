// Regenerates src/client/generated from the pinned OpenRails in e2e/server.
import { execFileSync } from "node:child_process"
import path from "node:path"

import { startPostgres, stopPostgres } from "../support/postgres.ts"

const root = path.resolve(import.meta.dirname, "../..")
const pg = await startPostgres()
try {
  execFileSync(
    "go",
    ["run", "./cmd/contract", "-out", path.join(root, "src/client/generated")],
    {
      cwd: path.join(root, "e2e/server"),
      env: { ...process.env, GOWORK: "off", DATABASE_URL: pg.dsn },
      stdio: "inherit",
    }
  )
} finally {
  stopPostgres(pg.name)
}
