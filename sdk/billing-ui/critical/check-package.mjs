import assert from "node:assert/strict"
import { execFileSync } from "node:child_process"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import { pathToFileURL } from "node:url"

// Exercise the packed artifact using the already-built dist. This command
// deliberately does no build and runs no second source/browser suite.
const root = path.resolve(import.meta.dirname, "..")
const temp = fs.mkdtempSync(path.join(os.tmpdir(), "openrails-sdk-package-"))
try {
  execFileSync("pnpm", ["pack", "--pack-destination", temp], {
    cwd: root,
    stdio: "pipe",
  })
  const archives = fs.readdirSync(temp).filter((name) => name.endsWith(".tgz"))
  assert.equal(archives.length, 1)
  execFileSync("tar", ["-xzf", path.join(temp, archives[0]), "-C", temp])
  const packed = path.join(temp, "package")
  const manifest = JSON.parse(
    fs.readFileSync(path.join(packed, "package.json"), "utf8")
  )
  function check(value) {
    if (typeof value === "object") {
      for (const nested of Object.values(value)) check(nested)
      return
    }
    if (typeof value !== "string") return
    const targets = value.includes("*")
      ? ["en", "de", "es", "ja", "ko", "zh"].map((locale) =>
          value.replaceAll("*", locale)
        )
      : [value]
    for (const target of targets)
      assert.ok(
        fs.existsSync(path.join(packed, target)),
        `missing packed export ${target}`
      )
  }
  check(manifest.exports)
  assert.ok(
    !fs.existsSync(path.join(packed, "critical")),
    "fixture drivers must not enter the published artifact"
  )
  fs.symlinkSync(
    path.join(root, "node_modules"),
    path.join(packed, "node_modules"),
    "dir"
  )
  const client = await import(
    pathToFileURL(path.join(packed, "dist/client.js")).href
  )
  assert.equal(typeof client.createBillingClient, "function")
  assert.equal(typeof client.tokenSource, "function")
  assert.equal(client.createBillingClient().currencies.USD, 6)
  assert.equal(client.decimalToAmount("9.99", 6), "9990000")
  console.log(
    "Packed SDK exports, declarations, assets and exact-money public import passed."
  )
} finally {
  fs.rmSync(temp, { recursive: true, force: true })
}
