// @vitest-environment jsdom
// Staff who sign in at a trusted issuer: a stale sign-in re-authorizes at the
// issuer and the write runs again; invitations are listed and accepted; an
// owner invites and revokes by email.
import { QueryClientProvider } from "@tanstack/react-query"
import { afterEach, expect, it, vi } from "vitest"

import { PendingInvites } from "@/components/pending-invites"
import { FederatedTeamTab } from "@/pages/settings/federated-team"
import { api, bindStepUp } from "@/lib/api/client"
import { issuerStepUp } from "@/lib/session"
import { client, selectMerchant, server } from "@/test/harness"
import { act, browserEnvironment, click, mount, unmount } from "@/test/mount"

afterEach(async () => {
  bindStepUp(null)
  await unmount()
})

const stepUpRequired = () =>
  Response.json(
    {
      error: {
        type: "authorization_error",
        code: "step_up_required",
        metadata: { max_age: 0 },
      },
    },
    { status: 403 }
  )

it("re-authorizes at the issuer and runs a refused write again", async () => {
  browserEnvironment()
  let fresh = false
  const requests = await server({
    "POST /merchant/federated-grants": () =>
      fresh
        ? Response.json({ id: "fgr_1" }, { status: 201 })
        : stepUpRequired(),
  })
  const stepUp = vi.fn(async () => {
    fresh = true
  })
  bindStepUp(issuerStepUp({ stepUp }))
  await expect(
    api("/merchant/federated-grants", {
      method: "POST",
      body: { email: "a@example.com", role: "viewer" },
    })
  ).resolves.toEqual({ id: "fgr_1" })
  expect(stepUp).toHaveBeenCalledWith({ popup: true })
  expect(
    requests.filter((r) => r.path === "/merchant/federated-grants")
  ).toHaveLength(2)
})

it("lists invitations and accepts one", async () => {
  browserEnvironment()
  const requests = await server({
    "GET /merchants/invites": {
      data: [
        {
          id: "fgr_1",
          merchant: { id: "m1", slug: "shop", display_name: "Shop" },
          role: "viewer",
        },
      ],
      next_cursor: null,
    },
    "POST /merchants/invites/fgr_1/accept": {
      id: "m1",
      slug: "shop",
      display_name: "Shop",
      role: "viewer",
      permissions: [],
    },
  })
  await mount(
    <QueryClientProvider client={client()}>
      <PendingInvites />
    </QueryClientProvider>
  )
  await act(async () => {})
  expect(document.body.textContent).toContain("Shop")
  await click("Accept")
  await act(async () => {})
  expect(requests.map((r) => `${r.method} ${r.path}`)).toContain(
    "POST /merchants/invites/fgr_1/accept"
  )
})

it("invites by email and revokes", async () => {
  browserEnvironment()
  selectMerchant("shop")
  const requests = await server({
    "GET /merchant/federated-grants": {
      data: [
        {
          id: "fgr_9",
          email: "old@example.com",
          role: "support",
          issuer: "https://idp.example",
          subject: "u9",
          accepted_at: "2026-10-01T00:00:00Z",
          created_at: "2026-09-30T00:00:00Z",
        },
      ],
      next_cursor: null,
    },
    "POST /merchant/federated-grants": () =>
      Response.json(
        {
          id: "fgr_10",
          email: "new@example.com",
          role: "viewer",
          issuer: null,
          subject: null,
          accepted_at: null,
          created_at: "2026-10-08T00:00:00Z",
        },
        { status: 201 }
      ),
    "DELETE /merchant/federated-grants/fgr_9": () =>
      new Response(null, { status: 204 }),
  })
  await mount(
    <QueryClientProvider client={client()}>
      <FederatedTeamTab />
    </QueryClientProvider>
  )
  await act(async () => {})
  expect(document.body.textContent).toContain("old@example.com")
  await act(async () => {
    const input = document.querySelector<HTMLInputElement>(
      "#federated-invite-email"
    )!
    const setValue = Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value"
    )!.set!
    setValue.call(input, "new@example.com")
    input.dispatchEvent(new Event("input", { bubbles: true }))
  })
  await click("Invite")
  await click("Revoke")
  await act(async () => {})
  const calls = requests.map((r) => `${r.method} ${r.path}`)
  expect(calls).toContain("POST /merchant/federated-grants")
  expect(calls).toContain("DELETE /merchant/federated-grants/fgr_9")
  expect(requests.find((r) => r.method === "POST")?.body).toEqual({
    email: "new@example.com",
    role: "viewer",
  })
})
