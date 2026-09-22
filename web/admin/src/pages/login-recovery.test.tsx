// @vitest-environment jsdom
import { StrictMode } from "react"
import { QueryClientProvider } from "@tanstack/react-query"
import { MemoryRouter } from "react-router-dom"
import { afterEach, beforeEach, expect, it } from "vitest"

import { AuthProvider } from "@/lib/auth"
import { getTokens } from "@/lib/api/client"
import { queryClient } from "@/lib/query-client"
import { LoginPage } from "@/pages/login"
import { server, type Reply } from "@/test/harness"
import { act, browserEnvironment, click, mount, unmount } from "@/test/mount"

const proof = "recovery-proof-never-store-as-a-session-12345"
const recovery = () => ({
  token: proof,
  expires_at: new Date(Date.now() + 60_000).toISOString(),
  purge_at: new Date(Date.now() + 86400_000).toISOString(),
})
const recoveryError = () =>
  Response.json(
    {
      error: {
        code: "account_recovery_required",
        message: "Recovery required",
        metadata: { recovery: recovery() },
      },
    },
    { status: 409 }
  )

beforeEach(() => {
  browserEnvironment()
  queryClient.clear()
  history.replaceState(null, "", "/admin/login")
})
afterEach(async () => {
  await unmount()
  queryClient.clear()
})

async function screen(routes: Record<string, Reply> = {}) {
  const requests = await server({
    "/capabilities": { password: { login: true } },
    ...routes,
  })
  await mount(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <AuthProvider>
            <LoginPage />
          </AuthProvider>
        </MemoryRouter>
      </QueryClientProvider>
    </StrictMode>
  )
  return requests
}

async function fill(id: string, value: string) {
  await act(async () => {
    const input = document.getElementById(id) as HTMLInputElement
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value"
    )!.set!.call(input, value)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  })
}

async function signIn() {
  await fill("login", "operator@example.test")
  await fill("password", "current-password")
  await click("Sign in")
}

it("requires explicit confirmation after password proof and returns to fresh sign-in", async () => {
  const requests = await screen({
    "/password/login": recoveryError,
    "/account/recovery/confirm": () => new Response(null, { status: 204 }),
  })
  await signIn()
  expect(document.body.textContent).toContain("Restore your account?")
  expect(document.body.textContent).toContain(
    "You can recover this account until"
  )
  expect(
    requests.filter((r) => r.path === "/account/recovery/confirm")
  ).toHaveLength(0)
  expect(
    JSON.stringify(
      queryClient
        .getMutationCache()
        .getAll()
        .map((m) => m.state)
    )
  ).not.toContain(proof)
  await click("Restore account")
  expect(
    requests
      .filter((r) => r.path === "/account/recovery/confirm")
      .map((r) => r.body)
  ).toEqual([{ token: proof }])
  expect(document.body.textContent).toContain(
    "Your account has been restored. Sign in to continue."
  )
  expect(document.getElementById("password")).toHaveProperty("value", "")
  expect(getTokens()).toBeNull()
  expect(requests.some((r) => r.path === "/me")).toBe(false)
})

it("waits for the normal second factor before showing recovery", async () => {
  const requests = await screen({
    "/password/login": () =>
      Response.json(
        {
          error: {
            code: "2fa_required",
            metadata: {
              user_id: "user",
              challenge: "challenge",
              method: "totp",
              default_factor: { id: "totp-1", method: "totp" },
              available_factors: [{ id: "totp-1", method: "totp" }],
            },
          },
        },
        { status: 403 }
      ),
    "/2fa/verify": recoveryError,
  })
  await signIn()
  expect(document.body.textContent).toContain("Enter your verification code")
  expect(document.body.textContent).not.toContain("Restore account")
  await fill("code", "123456")
  await click("Verify")
  expect(requests.find((r) => r.path === "/2fa/verify")?.body).toMatchObject({
    factor_id: "totp-1",
    code: "123456",
  })
  expect(document.body.textContent).toContain("Restore account")
  await click("Cancel")
  expect(document.body.textContent).not.toContain("Restore account")
  expect(requests.some((r) => r.path === "/account/recovery/confirm")).toBe(
    false
  )
  expect(getTokens()).toBeNull()
})

it("takes provider recovery from the fragment, clears history, and does not adopt mixed tokens", async () => {
  history.replaceState(
    null,
    "",
    "/admin/login?next=console#" +
      new URLSearchParams({
        error: "account_recovery_required",
        recovery: JSON.stringify(recovery()),
        access_token: "must-not-adopt",
      })
  )
  await screen()
  expect(window.location.hash).toBe("")
  expect(window.location.search).toBe("?next=console")
  expect(document.body.textContent).toContain("Restore account")
  expect(getTokens()).toBeNull()
  expect(sessionStorage.length).toBe(0)
  await click("Cancel")
  expect(document.body.textContent).not.toContain("Restore account")
})

it("discards expired provider proofs and failed confirmations without automatic retries", async () => {
  history.replaceState(
    null,
    "",
    "/admin/login#" +
      new URLSearchParams({
        error: "account_recovery_required",
        recovery: JSON.stringify({
          ...recovery(),
          expires_at: "2000-01-01T00:00:00Z",
        }),
      })
  )
  const requests = await screen({
    "/password/login": recoveryError,
    "/account/recovery/confirm": () =>
      Response.json(
        { error: { code: "invalid_credentials" } },
        { status: 401 }
      ),
  })
  expect(window.location.hash).toBe("")
  expect(document.body.textContent).toContain("invalid or expired")
  await signIn()
  await click("Restore account")
  expect(document.body.textContent).toContain(
    "Recovery could not be confirmed. Sign in again"
  )
  expect(document.body.textContent).not.toContain("Restore account")
  expect(
    requests.filter((r) => r.path === "/account/recovery/confirm")
  ).toHaveLength(1)
  expect(getTokens()).toBeNull()
})
