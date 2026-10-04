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

// One build serves any mount path (#1127): built URLs are relative to
// index.html's <base href>, which the Go handler points at the mount. Dev
// serves at the default /admin/.
export default defineConfig(({ command }) => ({
  base: command === "build" ? "./" : "/admin/",
  plugins: [react(), tailwindcss(), keepGoEmbedPlaceholder],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
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
