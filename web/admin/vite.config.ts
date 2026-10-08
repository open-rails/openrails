import fs from "fs"
import path from "path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig, type Plugin } from "vite"

// Served by the Go binary at admin_console.path: web/admin/embed.go go:embeds
// dist/ (`task admin-build`). Only dist/.gitkeep is committed (#754) so the Go
// package compiles without a build; emptyOutDir deletes it, so restore it.
const keepGoEmbedPlaceholder: Plugin = {
  name: "keep-go-embed-placeholder",
  apply: "build",
  writeBundle({ dir }) {
    if (dir) fs.writeFileSync(path.join(dir, ".gitkeep"), "")
  },
}

const consoleSrc = path.resolve(__dirname, "./src")

// A host build (scripts/build-admin-console.sh --extensions) leaves
// console-host.json beside this file: the host's src, where its own "@/"
// imports resolve, and the directories Tailwind scans for the host's classes.
const host: { src?: string; sources: string[] } = (() => {
  const file = path.resolve(__dirname, "console-host.json")
  if (!fs.existsSync(file)) return { sources: [] }
  const { src, sources = [] } = JSON.parse(fs.readFileSync(file, "utf8")) as {
    src?: string
    sources?: string[]
  }
  return { src: src ? path.resolve(src) : undefined, sources }
})()
const hostSrc = host.src

const within = (file: string, dir: string) =>
  file === dir || file.startsWith(dir + path.sep)

// "@/x" is the importing file's own src: the console's for the console's
// files, the host's (--extensions-src) for everything the host contributed.
const srcAlias: Plugin = {
  name: "console-src-alias",
  enforce: "pre",
  resolveId(source, importer) {
    if (!source.startsWith("@/")) return null
    const root =
      hostSrc && importer && !within(importer, consoleSrc)
        ? hostSrc
        : consoleSrc
    return this.resolve(path.join(root, source.slice(2)), importer, {
      skipSelf: true,
    })
  },
}

const hostTailwindSource: Plugin = {
  name: "console-host-tailwind-source",
  enforce: "pre",
  transform(code, id) {
    if (
      host.sources.length === 0 ||
      id !== path.join(consoleSrc, "index.css")
    ) {
      return null
    }
    const sources = host.sources.map((dir) => `@source ${JSON.stringify(dir)};`)
    return `${sources.join("\n")}\n${code}`
  },
}

// One build serves any mount path (#1127): built URLs are relative to
// index.html's <base href>, which the Go handler points at the mount. Dev
// serves at the default /admin/.
export default defineConfig(({ command }) => ({
  base: command === "build" ? "./" : "/admin/",
  plugins: [
    srcAlias,
    hostTailwindSource,
    react(),
    tailwindcss(),
    keepGoEmbedPlaceholder,
  ],
  resolve: {
    alias: {
      "@openrails/console": path.join(consoleSrc, "extensions/public.ts"),
    },
    // Host extension files resolve their own dependencies from the host's
    // node_modules; these must be the console's single copies.
    dedupe: [
      "react",
      "react-dom",
      "react-router-dom",
      "@tanstack/react-query",
      "@openrails/auth-ui",
      "@hugeicons/react",
    ],
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  server: {
    // Local dev against a running openrails: `pnpm run dev` proxies API + auth.
    proxy: {
      "/v1": "http://localhost:3053",
      "/auth": "http://localhost:3053",
      "/admin/config.json": "http://localhost:3053",
    },
  },
}))
