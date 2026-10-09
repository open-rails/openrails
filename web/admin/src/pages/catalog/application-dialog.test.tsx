// @vitest-environment jsdom
import { QueryClientProvider } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it } from "vitest"
import { client, selectMerchant, server, type Recorded } from "@/test/harness"
import { act, browserEnvironment, click, mount, unmount } from "@/test/mount"
import { CatalogApplicationDialog } from "./application-dialog"

let requests: Recorded[]
let response: () => Response
let revision: number
let writesAllowed: boolean
const receipt = (replayed = false) =>
  Response.json({
    application_id: "sha256:applied-batch",
    base_revision: 7,
    applied_revision: 8,
    replayed,
    products_changed: 1,
    prices_changed: 0,
  })
beforeEach(async () => {
  browserEnvironment()
  revision = 7
  writesAllowed = true
  response = () => receipt()
  requests = await server({
    "GET /admin/catalog/revision": () => ({
      revision,
      writes_allowed: writesAllowed,
    }),
    "POST /admin/catalog/applications": () => response(),
  })
  selectMerchant("merchant-one")
  await mount(
    <QueryClientProvider client={client()}>
      <CatalogApplicationDialog />
    </QueryClientProvider>
  )
})
afterEach(unmount)
const editor = () =>
  document.querySelector<HTMLTextAreaElement>("#catalog-application")!
const applications = () =>
  requests.filter((r) => r.path.endsWith("/applications"))
const reads = () => requests.filter((r) => r.path.endsWith("/revision"))
async function applyDraft() {
  await click("Apply catalog")
  await click("Review batch")
  await click("Apply reviewed batch")
}
describe("catalog batch retries", () => {
  it("retries the unchanged document after an uncertain response without caller metadata", async () => {
    response = () =>
      Response.json(
        { error: { message: "Connection outcome unknown" } },
        { status: 503 }
      )
    await applyDraft()
    const original = editor().value
    expect(editor().disabled).toBe(true)
    revision = 11
    response = () => receipt(true)
    await click("Retry exact batch")
    expect(applications()).toHaveLength(2)
    expect(applications()[0].body).toEqual(applications()[1].body)
    expect(applications()[0].headers.get("Content-Type")).toBe(
      "application/yaml"
    )
    expect(editor().value).toBe(original)
    expect(reads()).toHaveLength(1)
    expect(document.body.textContent).toContain("No changes were repeated")
    expect(document.body.textContent).toContain(
      "later catalog edits were preserved"
    )
    await click("New batch")
    const next = JSON.parse(editor().value)
    expect(next).toEqual({ schema_version: 1, prune: false, products: {} })
    expect(next).toEqual(JSON.parse(original))
    expect(reads()).toHaveLength(2)
  })
  it("allows editing a refused batch without fetching a catalog revision", async () => {
    response = () =>
      Response.json(
        { error: { code: "invalid_request", message: "invalid price amount" } },
        { status: 422 }
      )
    await applyDraft()
    const original = editor().value
    expect(reads()).toHaveLength(1)
    expect(document.body.textContent).toContain("Batch refused")
    await click("Edit batch")
    expect(editor().disabled).toBe(false)
    expect(editor().value).toBe(original)
    expect(applications()).toHaveLength(1)
    expect(reads()).toHaveLength(1)
    expect(document.body.textContent).not.toContain("Review complete")
  })
  it("does not prepare a mutation when the runtime capability is disabled", async () => {
    writesAllowed = false
    await click("Apply catalog")
    expect(editor().value).toBe("")
    expect(document.body.textContent).toContain("Catalog updates are disabled")
    expect(applications()).toHaveLength(0)
  })
  it("does not apply a draft to a newly selected merchant", async () => {
    await click("Apply catalog")
    await click("Review batch")
    selectMerchant("merchant-two")
    await click("Apply reviewed batch")
    expect(applications()).toHaveLength(0)
    expect(document.body.textContent).toContain("another selected merchant")
  })
  it("does not replace an unfinished document when the dialog closes", async () => {
    await click("Apply catalog")
    const original = editor().value
    await act(async () => {
      document.dispatchEvent(
        new KeyboardEvent("keydown", { key: "Escape", bubbles: true })
      )
    })
    await click("Apply catalog")
    expect(editor().value).toBe(original)
    expect(reads()).toHaveLength(1)
  })
})
