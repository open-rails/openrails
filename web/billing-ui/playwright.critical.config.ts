import { defineConfig, devices } from "@playwright/test"

export default defineConfig({
  testDir: "critical",
  testMatch: "client.spec.ts",
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env.CI,
  timeout: 45_000,
  outputDir: process.env.OPENRAILS_BROWSER_RESULTS,
  reporter: [
    ["list"],
    ["json", { outputFile: process.env.OPENRAILS_BROWSER_REPORT }],
  ],
  use: {
    baseURL: process.env.OPENRAILS_BROWSER_URL,
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
})
