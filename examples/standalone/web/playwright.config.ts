import { defineConfig, devices } from "@playwright/test"

// Run by TestBrowser (../content_test.go), which serves the example with NMI
// played by nmimock and sets E2E_BASE_URL and E2E_USERS.
export default defineConfig({
  testDir: "e2e",
  workers: 1,
  expect: { timeout: 15_000 }, // a payment is a real round trip through OpenRails
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  use: { baseURL: process.env.E2E_BASE_URL, trace: "retain-on-failure" },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
})
