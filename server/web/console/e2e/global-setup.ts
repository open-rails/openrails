import { execFileSync, spawn, type ChildProcess } from "node:child_process"
import { mkdtempSync, rmSync } from "node:fs"
import { createServer } from "node:net"
import { tmpdir } from "node:os"
import path from "node:path"
import {
  startPostgres,
  stopPostgres,
  type Postgres,
} from "../../../../sdk/billing-ui/e2e/support/postgres"

const root = path.resolve(import.meta.dirname, "..")

export default async function globalSetup() {
  const work = mkdtempSync(path.join(tmpdir(), "openrails-console-e2e-"))
  let postgres: Postgres | undefined
  let server: ChildProcess | undefined
  const teardown = async () => {
    if (server && server.exitCode === null) {
      server.kill("SIGTERM")
      await new Promise((resolve) => server!.once("exit", resolve))
    }
    stopPostgres(postgres?.name)
    rmSync(work, { recursive: true, force: true })
  }
  try {
    execFileSync("bash", ["scripts/build-admin-console.sh"], {
      cwd: path.resolve(root, "../../.."),
      stdio: "inherit",
    })
    const binary = path.join(work, "console-server")
    execFileSync("go", ["build", "-o", binary, "."], {
      cwd: path.join(root, "e2e/server"),
      env: { ...process.env, GOWORK: "off" },
      stdio: "inherit",
    })
    postgres = await startPostgres()
    const port = await freePort()
    const baseURL = `http://localhost:${port}`
    server = spawn(
      binary,
      [
        "-addr",
        `127.0.0.1:${port}`,
        "-base-url",
        baseURL,
        "-assets",
        path.join(root, "dist"),
      ],
      {
        env: { ...process.env, DATABASE_URL: postgres.dsn },
        stdio: ["ignore", "inherit", "inherit"],
      }
    )
    await waitHealthy(`http://127.0.0.1:${port}/__test/health`, server)
    process.env.CONSOLE_E2E_BASE_URL = baseURL
    return teardown
  } catch (error) {
    await teardown()
    throw error
  }
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = createServer()
    server.once("error", reject)
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address() as { port: number }
      server.close(() => resolve(port))
    })
  })
}

async function waitHealthy(url: string, server: ChildProcess) {
  const deadline = Date.now() + 120_000
  while (Date.now() < deadline) {
    if (server.exitCode !== null)
      throw new Error(`Console fixture exited (${server.exitCode})`)
    try {
      if ((await fetch(url)).ok) return
    } catch {
      /* Not listening yet. */
    }
    await new Promise((resolve) => setTimeout(resolve, 250))
  }
  throw new Error("Console fixture did not become healthy")
}
