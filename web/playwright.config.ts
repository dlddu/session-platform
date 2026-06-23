import { defineConfig, devices } from "@playwright/test";

// Playwright drives the *deployed* SUT (kind, deploy/) — or any control-plane
// reachable at E2E_BASE_URL (default http://localhost:8080, where the embedded
// SPA and the /api/v1 surface are served on one port). The suites assume the
// SUT is already up (`make e2e-up`), so there is no managed webServer here.
const baseURL = process.env.E2E_BASE_URL ?? "http://localhost:8080";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  // The α SUT is a single in-memory-stub replica; retry flakes on CI, serialize
  // to keep the shared state predictable.
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
  use: {
    baseURL,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
