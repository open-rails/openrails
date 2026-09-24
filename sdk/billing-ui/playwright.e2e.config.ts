import { defineConfig, devices } from "@playwright/test"

// Real AuthKit + OpenRails suite. E2E_BASE_URL is set by global setup.
export default defineConfig({
  testDir: "e2e/openrails",
  globalSetup: "./e2e/support/global-setup.ts",
  workers: 2,
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  use: {
    baseURL: process.env.E2E_BASE_URL,
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
})
