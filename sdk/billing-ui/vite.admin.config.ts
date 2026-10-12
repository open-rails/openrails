import path from "node:path"
import { fileURLToPath } from "node:url"
import react from "@vitejs/plugin-react"
import tailwindcss from "@tailwindcss/vite"
import { defineConfig } from "vite"
import { isolateCss } from "./tooling/scoped-css/isolate"

const root = path.dirname(fileURLToPath(import.meta.url))

// A separate build keeps admin modules and styles out of the customer entry.
export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    {
      name: "billing-admin-css",
      enforce: "post",
      async generateBundle(_options, bundle) {
        for (const asset of Object.values(bundle)) {
          if (asset.type === "asset" && asset.fileName.endsWith(".css"))
            asset.source = (
              await isolateCss(String(asset.source))
            ).replaceAll("orck", "orba")
        }
      },
    },
  ],
  resolve: { alias: { "#orck": path.resolve(root, "src") } },
  build: {
    emptyOutDir: false,
    sourcemap: true,
    lib: {
      entry: path.resolve(root, "src/admin/index.ts"),
      formats: ["es"],
      fileName: () => "admin.js",
      cssFileName: "admin",
    },
    rollupOptions: {
      external: (id) =>
        !id.startsWith(".") &&
        !id.startsWith("/") &&
        !id.startsWith("#orck") &&
        !id.startsWith("\0") &&
        !id.endsWith(".css"),
      output: { chunkFileNames: "admin-[name]-[hash].js" },
    },
  },
})
