import fs from "node:fs"
import path from "node:path"
import react from "@vitejs/plugin-react"
import { defineConfig, type Plugin } from "vite"

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
  server: {
    proxy: {
      "/v1": "http://localhost:3053",
      "/auth": "http://localhost:3053",
      "/admin/config.json": "http://localhost:3053",
    },
  },
}))
