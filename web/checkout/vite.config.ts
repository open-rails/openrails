import fs from "fs"
import path from "path"
import react from "@vitejs/plugin-react"
import { defineConfig, type Plugin } from "vite"

// embed.go embeds dist/; only dist/.gitkeep is committed so Go compiles
// without a build. emptyOutDir deletes it, so restore it.
const keepGoEmbedPlaceholder: Plugin = {
  name: "keep-go-embed-placeholder",
  apply: "build",
  writeBundle({ dir }) {
    if (dir) fs.writeFileSync(path.join(dir, ".gitkeep"), "")
  },
}

// The page owns its origin: index.html at /c/{order_id}, files under /assets/.
export default defineConfig({
  base: "/",
  plugins: [react(), keepGoEmbedPlaceholder],
  build: { outDir: "dist", emptyOutDir: true, assetsDir: "assets" },
})
