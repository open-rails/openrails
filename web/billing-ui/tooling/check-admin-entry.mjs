import assert from "node:assert/strict"
import { readFileSync, existsSync } from "node:fs"
import path from "node:path"

const dist = path.resolve(import.meta.dirname, "../dist")
const seen = new Set()
function inspect(file) {
  if (seen.has(file)) return
  seen.add(file)
  const code = readFileSync(file, "utf8")
  assert(
    !path.basename(file).startsWith("admin"),
    `Customer entry imports ${file}`
  )
  assert(
    !code.includes("data-orba-theme"),
    `Customer entry includes admin styles: ${file}`
  )
  const sourceMap = `${file}.map`
  if (existsSync(sourceMap)) {
    const { sources } = JSON.parse(readFileSync(sourceMap, "utf8"))
    assert(
      !sources.some((source) => /(?:^|\/)src\/admin\//.test(source)),
      `Customer entry bundles admin source in ${file}`
    )
  }
  for (const match of code.matchAll(
    /(?:from\s*|import\s*\(|import\s*)["'](\.\.?\/[^"']+\.js)["']/g
  ))
    inspect(path.resolve(path.dirname(file), match[1]))
}
for (const entry of ["index.js", "client.js", "react.js"])
  inspect(path.join(dist, entry))
assert(
  !readFileSync(path.join(dist, "styles.css"), "utf8").includes(".orba"),
  "Customer stylesheet includes admin rules"
)
assert(existsSync(path.join(dist, "admin.js")), "Admin entry is missing")
assert(
  readFileSync(path.join(dist, "admin.css"), "utf8").includes(".orba"),
  "Admin stylesheet is not scoped"
)
console.log(
  `Customer entries are separate from admin (${seen.size} JavaScript chunks inspected).`
)
