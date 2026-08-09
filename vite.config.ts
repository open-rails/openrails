import path from "node:path"
import { fileURLToPath } from "node:url"

import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"
import dts from "vite-plugin-dts"

import { checkoutCssPlugin } from "./tooling/checkout-css/vite-plugin.ts"

const root = path.dirname(fileURLToPath(import.meta.url))

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    dts({
      include: ["src"],
      bundleTypes: true,
      tsconfigPath: path.resolve(root, "tsconfig.json"),
    }),
    checkoutCssPlugin(),
  ],
  resolve: {
    alias: {
      "#orck": path.resolve(root, "src"),
    },
  },
  build: {
    lib: {
      entry: path.resolve(root, "src/index.ts"),
      formats: ["es"],
      fileName: "index",
      cssFileName: "styles",
    },
    sourcemap: true,
    rollupOptions: {
      external: [
        "@base-ui/react",
        "@base-ui/react/button",
        "@base-ui/react/dialog",
        "@base-ui/react/input",
        "@base-ui/react/radio",
        "@base-ui/react/radio-group",
        "@hugeicons/core-free-icons",
        "@hugeicons/react",
        "class-variance-authority",
        "clsx",
        "react",
        "react-dom",
        "react/jsx-runtime",
        "tailwind-merge",
        "zod",
      ],
    },
  },
})
