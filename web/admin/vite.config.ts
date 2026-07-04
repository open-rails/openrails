import path from "path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// Served by the Go binary at /admin/. Build via scripts/build-admin-console.sh
// (in-repo: `task admin-build`), which overrides --outDir; dist is NEVER
// committed (#754) — whoever builds the binary go:embeds it.
export default defineConfig({
  base: "/admin/",
  plugins: [react(), tailwindcss()],
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
    // Local dev against a running openrails: `npm run dev` proxies API + auth.
    proxy: {
      "/v1": "http://localhost:3053",
      "/auth": "http://localhost:3053",
      "/admin/config.json": "http://localhost:3053",
    },
  },
})
