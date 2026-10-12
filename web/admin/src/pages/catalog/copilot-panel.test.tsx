// @vitest-environment jsdom
import { QueryClientProvider } from "@tanstack/react-query"
import { afterEach, expect, it } from "vitest"
import { client, selectMerchant, server } from "@/test/harness"
import { act, browserEnvironment, click, mount, unmount } from "@/test/mount"
import { CatalogCopilotPanel } from "./copilot-panel"

afterEach(unmount)

it("asks the console assistant and renders its answer", async () => {
  browserEnvironment()
  const requests = await server({
    "POST /console/catalog/ask": {
      answer: "Premium has a monthly price.",
      evidence: [],
      drafts: [],
    },
  })
  selectMerchant("merchant-one")
  await mount(
    <QueryClientProvider client={client()}>
      <CatalogCopilotPanel enabled draftingEnabled={false} />
    </QueryClientProvider>
  )
  const input = document.querySelector("input")!
  await act(async () => {
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value"
    )!.set!.call(input, "What do we sell?")
    input.dispatchEvent(new Event("input", { bubbles: true }))
  })
  await click("Ask")
  expect(document.body.textContent).toContain("Premium has a monthly price.")
  expect(requests.map((r) => `${r.method} ${r.path}`)).toEqual([
    "POST /console/catalog/ask",
  ])
  expect(requests[0].body).toEqual({ question: "What do we sell?" })
})

it("makes no request when the assistant capability is disabled", async () => {
  browserEnvironment()
  const requests = await server({})
  selectMerchant("merchant-one")
  await mount(
    <QueryClientProvider client={client()}>
      <CatalogCopilotPanel enabled={false} draftingEnabled={false} />
    </QueryClientProvider>
  )
  expect(document.querySelector("input")).toBeNull()
  expect(requests).toEqual([])
})
