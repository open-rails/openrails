import { execFileSync, spawn, type ChildProcess } from "node:child_process"
import { mkdtempSync, rmSync } from "node:fs"
import { createServer } from "node:net"
import { tmpdir } from "node:os"
import path from "node:path"

const page = path.resolve(import.meta.dirname, "..")
const serverModule = path.resolve(page, "../../server")
const IMAGE = process.env.E2E_POSTGRES_IMAGE ?? "postgres:18-alpine"

// Builds the page and the harness, then starts the harness on a fresh
// Postgres; teardown stops both.
export default async function globalSetup() {
  const work = mkdtempSync(path.join(tmpdir(), "checkout-e2e-"))
  const pg = `checkout-e2e-pg-${process.pid}-${Date.now()}`
  let harness: ChildProcess | undefined
  const teardown = async () => {
    if (harness && harness.exitCode === null) {
      harness.kill("SIGTERM")
      await new Promise((r) => harness!.once("exit", r))
    }
    try {
      execFileSync("docker", ["rm", "-f", pg], { stdio: "ignore" })
    } catch {
      // already gone
    }
    rmSync(work, { recursive: true, force: true })
  }
  try {
    // The page from this checkout's billing-ui, as the image builds it.
    execFileSync("bash", [path.resolve(page, "../../scripts/build-checkout-page.sh")], { stdio: "inherit" })
    const bin = path.join(work, "checkoutpage")
    execFileSync("go", ["build", "-tags", "e2e,integration", "-o", bin, "./ci/checkoutpage"], {
      cwd: serverModule,
      stdio: "inherit",
    })
    execFileSync(
      "docker",
      ["run", "-d", "--rm", "--name", pg, "-e", "POSTGRES_PASSWORD=postgres", "-e", "POSTGRES_DB=checkout", "-p", "127.0.0.1::5432", IMAGE],
      { stdio: ["ignore", "ignore", "inherit"] }
    )
    const pgPort = execFileSync("docker", ["port", pg, "5432/tcp"], { encoding: "utf8" }).trim().split("\n")[0].split(":").pop()
    await until(() => {
      execFileSync("docker", ["exec", pg, "pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-d", "checkout"], { stdio: "ignore" })
    })
    const port = await freePort()
    harness = spawn(bin, ["-addr", `127.0.0.1:${port}`, "-assets", path.join(page, "dist"), "-lifetime", "30m"], {
      env: { ...process.env, DATABASE_URL: `postgres://postgres:postgres@127.0.0.1:${pgPort}/checkout?sslmode=disable` },
      stdio: ["ignore", "inherit", "inherit"],
    })
    await until(async () => {
      if (harness!.exitCode !== null) throw new Error("harness exited")
      const res = await fetch(`http://localhost:${port}/__test/health`)
      if (!res.ok) throw new Error(`health ${res.status}`)
    })
    process.env.E2E_API = `http://localhost:${port}`
    return teardown
  } catch (err) {
    await teardown()
    throw err
  }
}

async function until(probe: () => unknown) {
  const deadline = Date.now() + 90_000
  for (;;) {
    try {
      await probe()
      return
    } catch (err) {
      if (Date.now() > deadline) throw err
      await new Promise((r) => setTimeout(r, 500))
    }
  }
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer()
    srv.listen(0, "127.0.0.1", () => {
      const address = srv.address()
      srv.close(() => (typeof address === "object" && address ? resolve(address.port) : reject(new Error("no port"))))
    })
  })
}
