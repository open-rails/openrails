import { defineConfig } from "@playwright/test"

const port = 4173

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  retries: 0,
  workers: 1,
  reporter: "line",
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    browserName: "chromium",
    trace: "retain-on-failure",
  },
  webServer: {
    command: `pnpm exec vite --host 0.0.0.0 --port ${port} --strictPort`,
    url: `http://127.0.0.1:${port}/e2e/fixtures/host.html`,
    reuseExistingServer: false,
    timeout: 30_000,
  },
})
