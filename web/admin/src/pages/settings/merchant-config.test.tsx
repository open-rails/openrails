// @vitest-environment jsdom
// The merchant's configuration pages against the real api client and query
// cache: read-only from a file, check-and-set edits, write-only credentials.
import { QueryClientProvider } from "@tanstack/react-query"
import type { ReactNode } from "react"
import { MemoryRouter } from "react-router-dom"
import { toast } from "sonner"
import { afterEach, expect, it, vi } from "vitest"

import type {
  AlertWebhook,
  MerchantConfigurationState,
  PSP,
  RailDefinition,
  UpdateMerchantConfigurationParams,
} from "@/lib/api/generated/wire"
import {
  calls,
  client,
  selectMerchant,
  server,
  type Recorded,
} from "@/test/harness"
import { act, browserEnvironment, choose, mount, unmount } from "@/test/mount"
import { MerchantSettingsTab, PSPsTab } from "./index"
import { NotificationsTab } from "./notifications"

afterEach(async () => {
  await unmount()
  vi.restoreAllMocks()
})

const WHEN = "2026-10-01T00:00:00Z"

const capabilities = (edits: () => boolean) => () => ({
  capabilities: {
    route_groups: { merchant_config: true },
    features: { merchant_config_edits: edits() },
  },
  rails,
  currencies: [],
  payment: null,
})

const configuration = (
  revision: number,
  displayName: string
): MerchantConfigurationState => ({
  revision,
  display_name: displayName,
  api_host: "",
  settings: {
    profile: { from_email: "billing@acme.test" },
    alert_email: "ops@acme.test",
    reprice_notice_window_days: 14,
  },
})

const aPSP = (revision = 7): PSP => ({
  id: "psp_a",
  key: "main",
  rail: "stripe",
  environment: "test",
  account_id: "acct_a",
  archived: false,
  open_obligations: 0,
  settings: {},
  credentials: {
    secret_key: { configured: true, validated_at: WHEN },
    webhook_secret: { configured: true, validated_at: null },
  },
  revision,
  created_at: WHEN,
  updated_at: WHEN,
})

const rails: RailDefinition[] = [
  {
    rail: "stripe",
    display_name: "Stripe",
    credential_keys: ["secret_key", "webhook_secret"],
    setting_keys: [],
  },
]

const webhook: AlertWebhook = {
  id: "awh_1",
  name: "ops",
  destination_host: "hooks.slack.com",
  format: "slack",
  enabled: true,
  created_at: WHEN,
  updated_at: WHEN,
}

const mismatch = () =>
  Response.json(
    { error: { code: "revision_mismatch", message: "changed" } },
    { status: 409 }
  )

async function page(node: ReactNode) {
  const queries = client()
  await mount(
    <QueryClientProvider client={queries}>
      <MemoryRouter>{node}</MemoryRouter>
    </QueryClientProvider>
  )
  await settle()
  return queries
}

const settle = () =>
  act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0))
  })

const labels = () =>
  [...document.querySelectorAll("button")].map(
    (node) => node.getAttribute("aria-label") ?? node.textContent!.trim()
  )

async function press(label: string, index = 0) {
  const found = [...document.querySelectorAll("button")].filter(
    (node) =>
      (node.getAttribute("aria-label") ?? node.textContent!.trim()) === label
  )[index]
  expect(found, `button ${label} #${index}`).toBeDefined()
  expect(found.disabled).toBe(false)
  await act(async () => found.click())
  await settle()
}

const field = (selector: string) => {
  const found = document.querySelector<HTMLInputElement>(selector)
  expect(found, selector).not.toBeNull()
  return found!
}

const type = (selector: string, value: string) =>
  act(async () => {
    const input = field(selector)
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value"
    )!.set!.call(input, value)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  })

const writes = (requests: Recorded[]) =>
  requests.filter((request) => request.method !== "GET")

it("a configuration read from a file shows every value and offers no edit", async () => {
  browserEnvironment()
  let edits = false
  const requests = await server({
    "/config": capabilities(() => edits),
    "GET /admin/configuration": configuration(3, "Acme Retail"),
    "GET /admin/psps": { data: [aPSP()], next_cursor: null },
    "GET /admin/alert-webhooks": { data: [webhook], next_cursor: null },
  })
  selectMerchant("merchant-a")
  const queries = await page(
    <>
      <MerchantSettingsTab />
      <NotificationsTab />
      <PSPsTab />
    </>
  )

  const text = document.body.textContent!
  for (const value of [
    "Acme Retail",
    "billing@acme.test",
    "14 days",
    "ops@acme.test",
    "hooks.slack.com",
    "acct_a",
    "secret_key",
  ])
    expect(text).toContain(value)
  expect(text).toContain(
    "Read-only: this merchant's configuration comes from a file"
  )
  expect(labels()).toEqual([])
  expect(writes(requests)).toEqual([])

  // The same pages offer every edit once Vault holds the configuration.
  edits = true
  await act(async () => {
    await queries.invalidateQueries({ queryKey: ["config"] })
  })
  await settle()
  expect(document.body.textContent).not.toContain("Read-only")
  expect(labels()).toEqual([
    "Edit",
    "Edit",
    "Edit",
    "Add webhook",
    "Edit",
    "Delete webhook",
    "Add PSP",
    "Rotate",
    "Archive",
  ])
})

it("a configuration edit names the revision it opened at; one changed since closes and reloads", async () => {
  browserEnvironment()
  const error = vi.spyOn(toast, "error")
  let current = configuration(3, "Acme Retail")
  const requests = await server({
    "/config": capabilities(() => true),
    "GET /admin/configuration": () => current,
    "PATCH /admin/configuration": (request) => {
      const body = request.body as UpdateMerchantConfigurationParams
      if (body.expected_revision !== current.revision) return mismatch()
      current = {
        ...current,
        revision: current.revision + 1,
        display_name: body.display_name ?? current.display_name,
      }
      return current
    },
  })
  selectMerchant("merchant-a")
  const queries = await page(<MerchantSettingsTab />)

  await press("Edit")
  // Another operator saves while this form is open, and a background refetch
  // brings the new revision in: the edit still names the one it opened at.
  current = configuration(4, "Acme Holdings")
  await act(async () => {
    await queries.invalidateQueries()
  })
  await settle()
  await type("#s-name", "Acme Corp")
  await press("Save")

  expect(writes(requests).map((request) => request.body)).toEqual([
    {
      expected_revision: 3,
      display_name: "Acme Corp",
      settings: { profile: { from_email: "billing@acme.test" } },
    },
  ])
  expect(error).toHaveBeenCalledWith(
    "The configuration changed since you opened it",
    expect.anything()
  )
  expect(document.querySelector("#s-name")).toBeNull()
  expect(calls(requests).at(-1)).toBe("GET /admin/configuration")
  expect(document.body.textContent).toContain("Acme Holdings")

  // Reopened, the form holds the current values and their revision.
  await press("Edit")
  expect(field("#s-name").value).toBe("Acme Holdings")
  await type("#s-name", "Acme Corp")
  await press("Save")
  expect(writes(requests).at(-1)!.body).toMatchObject({ expected_revision: 4 })
  expect(document.querySelector("#s-name")).toBeNull()
  expect(document.body.textContent).toContain("Acme Corp")
})

it("a PSP rotation at a stale revision closes, drops the secret and reloads the PSP", async () => {
  browserEnvironment()
  const error = vi.spyOn(toast, "error")
  let psp = aPSP(7)
  const requests = await server({
    "/config": capabilities(() => true),
    "GET /admin/psps": () => ({ data: [psp], next_cursor: null }),
    "PATCH /admin/psps/psp_a": (request) => {
      const body = request.body as { expected_revision: number }
      if (body.expected_revision !== psp.revision) return mismatch()
      psp = { ...psp, revision: psp.revision + 1 }
      return psp
    },
  })
  selectMerchant("merchant-a")
  const queries = await page(<PSPsTab />)

  await press("Rotate")
  psp = aPSP(8)
  await act(async () => {
    await queries.invalidateQueries()
  })
  await settle()
  await type("#rot-psp_a-secret_key", "sk_live_new")
  await press("Validate & rotate")

  expect(writes(requests).map((request) => request.body)).toEqual([
    { expected_revision: 7, credentials: { secret_key: "sk_live_new" } },
  ])
  expect(error).toHaveBeenCalledWith(
    "PSP main changed since you opened it",
    expect.anything()
  )
  expect(document.querySelector("#rot-psp_a-secret_key")).toBeNull()
  expect(calls(requests).at(-1)).toBe("GET /admin/psps")

  await press("Rotate")
  expect(field("#rot-psp_a-secret_key").value).toBe("")
  await type("#rot-psp_a-secret_key", "sk_live_new")
  await press("Validate & rotate")
  expect(writes(requests).at(-1)!.body).toEqual({
    expected_revision: 8,
    credentials: { secret_key: "sk_live_new" },
  })
  expect(document.querySelector("#rot-psp_a-secret_key")).toBeNull()

  // Archiving is the same check-and-set update.
  await press("Archive")
  expect(writes(requests).at(-1)).toMatchObject({
    method: "PATCH",
    path: "/admin/psps/psp_a",
    body: { archived: true, expected_revision: 9 },
  })
})

it("credentials and webhook URLs are write-only: never shown, sent only when typed, dropped on close", async () => {
  browserEnvironment()
  const requests = await server({
    "/config": capabilities(() => true),
    "GET /admin/configuration": configuration(3, "Acme Retail"),
    "GET /admin/psps": { data: [aPSP()], next_cursor: null },
    "GET /admin/alert-webhooks": { data: [webhook], next_cursor: null },
    "PATCH /admin/psps/psp_a": aPSP(8),
    "PATCH /admin/alert-webhooks/awh_1": { ...webhook, enabled: false },
  })
  selectMerchant("merchant-a")
  await page(
    <>
      <PSPsTab />
      <NotificationsTab />
    </>
  )

  // Both slots hold a credential, yet neither is shown or prefilled.
  await press("Rotate")
  for (const name of ["secret_key", "webhook_secret"]) {
    const slot = field(`#rot-psp_a-${name}`)
    expect(slot.type).toBe("password")
    expect(slot.value).toBe("")
    expect(slot.placeholder).toBe("unchanged")
  }
  await type("#rot-psp_a-secret_key", "sk_dismissed")
  await press("Close")
  await press("Rotate")
  expect(field("#rot-psp_a-secret_key").value).toBe("")

  // A blank slot keeps its credential: only the typed one is written.
  await type("#rot-psp_a-webhook_secret", "whsec_new")
  await press("Validate & rotate")
  expect(writes(requests).at(-1)!.body).toEqual({
    expected_revision: 7,
    credentials: { webhook_secret: "whsec_new" },
  })
  await press("Rotate")
  expect(field("#rot-psp_a-webhook_secret").value).toBe("")
  await press("Close")

  // A new PSP's credentials are password fields too.
  await press("Add PSP")
  await choose("Stripe")
  for (const name of ["secret_key", "webhook_secret"]) {
    const slot = field(`#pv-credential-${name}`)
    expect(slot.type).toBe("password")
    expect(slot.value).toBe("")
  }
  await press("Close")

  // The webhook URL carries the receiver's secret: the edit shows only its
  // host and leaves the URL out unless a new one is typed.
  await press("Edit", 1)
  expect(field("#wh-url").value).toBe("")
  expect(field("#wh-url").placeholder).toBe("unchanged")
  await act(async () => field("#wh-enabled").click())
  await press("Save")
  expect(writes(requests).at(-1)).toMatchObject({
    method: "PATCH",
    path: "/admin/alert-webhooks/awh_1",
  })
  expect(writes(requests).at(-1)!.body).toEqual({
    name: "ops",
    format: "slack",
    enabled: false,
  })
  expect(document.body.innerHTML).not.toContain("whsec_new")
  expect(document.body.innerHTML).not.toContain("sk_dismissed")
})

it("an edit refused because the configuration comes from a file turns the pages read-only", async () => {
  browserEnvironment()
  let edits = true
  const readOnly = () => {
    edits = false
    return Response.json(
      {
        error: {
          code: "merchant_config_read_only",
          message: "read from a file",
        },
      },
      { status: 409 }
    )
  }
  const requests = await server({
    "/config": capabilities(() => edits),
    "GET /admin/configuration": configuration(3, "Acme Retail"),
    "GET /admin/psps": { data: [aPSP()], next_cursor: null },
    "PATCH /admin/configuration": readOnly,
    "PATCH /admin/psps/psp_a": readOnly,
  })
  selectMerchant("merchant-a")
  const queries = await page(
    <>
      <MerchantSettingsTab />
      <PSPsTab />
    </>
  )

  await press("Edit")
  await type("#s-name", "Acme Corp")
  await press("Save")
  expect(document.querySelector("#s-name")).toBeNull()
  expect(labels()).toEqual([])
  expect(document.body.textContent).toContain("Read-only")

  // A PSP dialog left open when the configuration turned read-only closes
  // and drops what was typed.
  edits = true
  await act(async () => {
    await queries.invalidateQueries({ queryKey: ["config"] })
  })
  await settle()
  await press("Rotate")
  await type("#rot-psp_a-secret_key", "sk_live_new")
  await press("Validate & rotate")
  expect(document.querySelector("#rot-psp_a-secret_key")).toBeNull()
  expect(labels()).toEqual([])
  expect(document.body.innerHTML).not.toContain("sk_live_new")
  expect(writes(requests).map((request) => request.path)).toEqual([
    "/admin/configuration",
    "/admin/psps/psp_a",
  ])
})
