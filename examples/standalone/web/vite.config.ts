import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// Builds dist/, which the Go server serves: /assets, and index.html for every page.
export default defineConfig({ plugins: [react()] })
