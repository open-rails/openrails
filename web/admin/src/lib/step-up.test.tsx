// @vitest-environment jsdom
// OpenRails refuses an owner write whose sign-in is stale (403
// step_up_required). The console answers with AuthKit's own step-up dialog
// (auth-ui) and runs the write again on the fresh session; cancelling leaves
// OpenRails' refusal. The real api client, auth-ui client and dialog run
// against the stubbed server.
import { afterEach, beforeEach, expect, it } from "vitest"

import { api, ApiError } from "@/lib/api/client"
import { ConsoleSession } from "@/lib/session"
import { accessToken, server, session, type Recorded } from "@/test/harness"
import { act, browserEnvironment, click, mount, unmount } from "@/test/mount"

const fresh = accessToken("console-test", Math.floor(Date.now() / 1000) + 60)
const stepUpRequired = () =>
  Response.json(
    {
      error: {
        type: "invalid_request_error",
        code: "step_up_required",
        message: "step_up_required",
        metadata: { step_up_methods: ["password"], max_age_seconds: 900 },
      },
    },
    { status: 403 }
  )

let grants: Recorded[]
let stepUps: Recorded[]
beforeEach(async () => {
  browserEnvironment()
  grants = []
  stepUps = []
  await server({
    "POST /merchant/customers/cus_1/entitlements": (request) => {
      grants.push(request)
      return request.headers.get("Authorization") === `Bearer ${fresh}`
        ? Response.json({ id: "ent_1" }, { status: 201 })
        : stepUpRequired()
    },
    "POST /me/step-up/password": (request) => {
      stepUps.push(request)
      return {
        status: "complete",
        token_set: {
          access_token: fresh,
          token_type: "Bearer",
          expires_in: 3600,
        },
        fresh_auth: {
          last_authenticated_at: new Date().toISOString(),
          step_up_required_for_sensitive_actions: false,
          step_up_required_in_seconds: 900,
          auth_methods: ["password"],
        },
      }
    },
  })
  // The dialog loads apart from the entry chunk; have it ready, as a running
  // console does long before its first write.
  await import("@/lib/step-up-host")
  await mount(
    <ConsoleSession session={{ kind: "local", client: session }}>
      <span />
    </ConsoleSession>
  )
  await act(async () => {})
})
afterEach(unmount)

const grant = () =>
  api("/merchant/customers/cus_1/entitlements", {
    method: "POST",
    body: { entitlement: "premium", hours: 24 },
  })

async function dialogOpens() {
  await act(async () => {
    for (
      let i = 0;
      i < 50 && !document.querySelector('input[type="password"]');
      i++
    )
      await new Promise((resolve) => setTimeout(resolve, 10))
  })
  const password = document.querySelector<HTMLInputElement>(
    'input[type="password"]'
  )
  expect(password, "the step-up dialog asks for the password").not.toBeNull()
  expect(document.body.textContent).toContain("Confirm it's you")
  return password!
}

it("re-authenticates in AuthKit's dialog and retries the write", async () => {
  const write = grant()
  const password = await dialogOpens()
  await act(async () => {
    const setValue = Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value"
    )!.set!
    setValue.call(password, "Correct-horse-1")
    password.dispatchEvent(new Event("input", { bubbles: true }))
  })
  await click("Confirm")

  await act(async () => {
    await expect(write).resolves.toEqual({ id: "ent_1" })
  })
  expect(stepUps.map((request) => request.body)).toEqual([
    { password: "Correct-horse-1" },
  ])
  expect(grants).toHaveLength(2)
  expect(grants[1].body).toEqual({ entitlement: "premium", hours: 24 })
  expect(session.getAccessToken()).toBe(fresh)
})

it("leaves OpenRails' refusal when the user cancels", async () => {
  const write = grant().catch((error: unknown) => error)
  await dialogOpens()
  await click("Close")

  const refused = await write
  expect(refused).toBeInstanceOf(ApiError)
  expect((refused as ApiError).stepUpRequired).toBe(true)
  expect(grants).toHaveLength(1)
  expect(stepUps).toHaveLength(0)
})

it("never asks a read to step up", async () => {
  await expect(api("/merchant/customers/cus_1")).resolves.toEqual({})
  expect(document.querySelector('input[type="password"]')).toBeNull()
})
