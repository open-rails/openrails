import { defineConfig, devices } from "@playwright/test"

// The hosted checkout in a real browser against the standalone server
// (server/ci/checkoutpage) with the loopback NMI gateway. Global setup sets
// E2E_API and starts everything.
export default defineConfig({
  testDir: "e2e",
  globalSetup: "./e2e/global-setup.ts",
  workers: 1,
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  use: { trace: "retain-on-failure" },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
})
