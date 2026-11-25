import fs from "node:fs"
import path from "node:path"
import react from "@vitejs/plugin-react"
import type { Plugin } from "vite"
import { defineConfig } from "vite"

const keepGoEmbedPlaceholder: Plugin = {
  name: "keep-go-embed-placeholder",
  apply: "build",
  writeBundle({ dir }) {
    if (dir) fs.writeFileSync(path.join(dir, ".gitkeep"), "")
  },
}
export default defineConfig(({ command }) => ({
  base: command === "build" ? "./" : "/admin/",
  plugins: [react(), keepGoEmbedPlaceholder],
  resolve: {
    alias: { "@": path.resolve(__dirname, "src") },
    dedupe: [
      "react",
      "react-dom",
      "@tanstack/react-query",
      "@openrails/auth-ui",
    ],
  },
  build: { outDir: "dist", emptyOutDir: true },
  // `task dev` makes this origin the API's public URL, so the port is fixed.
  server: {
    port: 3054,
    strictPort: true,
    proxy: Object.fromEntries(
      [
        "/v1",
        "/auth",
        "/oauth2",
        "/.well-known",
        "/health",
        "/admin/config.json",
        "/admin/merchants",
      ].map((p) => [p, "http://localhost:3053"])
    ),
  },
}))
