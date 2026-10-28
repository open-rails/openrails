import { createRoot } from "react-dom/client"
import { BillingAdmin } from "@openrails/billing-ui/admin"

// The console as a host mounts it: its API through this origin's proxy to
// the real router, each request carrying the signed-in staff member's token.
// The spec picks the staff member (localStorage "critical-as").
export interface ConsoleFixture {
  tokens: Record<string, string>
  [key: string]: unknown
}
declare global {
  interface Window {
    consoleFixture: ConsoleFixture
  }
}
const fixture = (await (
  await fetch("/__fixture/config")
).json()) as ConsoleFixture
window.consoleFixture = fixture
const token = fixture.tokens[localStorage.getItem("critical-as") ?? "staff"]
const auth = {
  authFetch(input: string, init?: RequestInit) {
    const headers = new Headers(init?.headers)
    headers.set("Authorization", `Bearer ${token}`)
    return fetch(input, { ...init, headers })
  },
}
createRoot(document.getElementById("app")!).render(
  <BillingAdmin
    baseUrl={`${location.origin}/v1`}
    auth={auth}
    basePath="/console"
  />
)
