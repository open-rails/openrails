import { defineConfig, devices } from "@playwright/test"

// The console as the openrails binary serves it, driven by the critical Go
// suite (critical/console_first_run_test.go), which starts the server and
// passes its URL.
export default defineConfig({
  testDir: "critical",
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env.CI,
  timeout: 120_000,
  expect: { timeout: 20_000 },
  outputDir: process.env.OPENRAILS_BROWSER_RESULTS,
  reporter: [
    ["list"],
    ["json", { outputFile: process.env.OPENRAILS_BROWSER_REPORT }],
  ],
  use: {
    baseURL: process.env.OPENRAILS_BROWSER_URL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
})
