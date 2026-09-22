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
    application_id: "applied",
    catalog_id: "cat-one",
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
    "GET /merchant/catalog/revision": () => ({
      revision,
      writes_allowed: writesAllowed,
    }),
    "POST /merchant/catalog/applications": () => response(),
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
  await click("Review application")
  await click("Apply reviewed application")
}
describe("catalog application identity", () => {
  it("retries the unchanged ID, revision and document after an uncertain response", async () => {
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
    await click("Retry exact application")
    expect(applications()).toHaveLength(2)
    expect(applications()[0].body).toEqual(applications()[1].body)
    expect(applications()[0].headers.get("Content-Type")).toBe(
      "application/yaml"
    )
    expect(editor().value).toBe(original)
    expect(reads()).toHaveLength(1)
    expect(document.body.textContent).toContain("No changes were repeated")
    await click("New application")
    const next = JSON.parse(editor().value)
    expect(next.expected_revision).toBe(11)
    expect(next.application_id).not.toBe(JSON.parse(original).application_id)
    expect(reads()).toHaveLength(2)
  })
  it("keeps a conflict's original precondition until explicit review and editing", async () => {
    response = () =>
      Response.json(
        { error: { code: "resource_conflict", message: "revision changed" } },
        { status: 409 }
      )
    await applyDraft()
    const original = editor().value
    revision = 9
    expect(reads()).toHaveLength(1)
    expect(document.body.textContent).toContain(
      "expected revision have not changed"
    )
    await click("Review current revision")
    expect(editor().value).toBe(original)
    expect(document.body.textContent).toContain("Current revision: 9")
    await click("Edit application")
    expect(editor().disabled).toBe(false)
    expect(editor().value).toBe(original)
    expect(applications()).toHaveLength(1)
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
    await click("Review application")
    selectMerchant("merchant-two")
    await click("Apply reviewed application")
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
