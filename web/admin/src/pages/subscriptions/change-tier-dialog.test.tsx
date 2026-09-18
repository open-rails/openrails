// @vitest-environment jsdom
import { act } from "react"
import { createRoot, type Root } from "react-dom/client"
import {
  QueryClient,
  QueryClientProvider,
  notifyManager,
} from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { loadBootstrap, setTokens } from "@/lib/api/client"
import { adminQueries } from "@/lib/queries"
import { ChangeTierDialog } from "./change-tier-dialog"

const preview = {
  object: "tier_change_preview",
  action: "upgrade",
  price_id: "price-pro",
  rail: "stripe",
  currency: "USD",
  amount_due_now: "12000000",
  next_charge_amount: "20000000",
  effective: "now",
  is_estimate: false,
}
const result = (status: string) =>
  Response.json(
    {
      object: "tier_change",
      mode: "tier_change",
      action: "upgrade",
      price_id: "price-pro",
      payment: { rail: "stripe" },
      status,
    },
    { status: status === "processing" ? 202 : 200 }
  )
const decline = (code = "stripe_card_declined") =>
  Response.json(
    {
      error: { code, message: "The card was declined" },
    },
    { status: 402 }
  )

let root: Root
let client: QueryClient
let requests: { key: string; price: string }[]
let answer: () => Promise<Response>
let previewAnswer: () => Promise<Response>

function pendingResponse() {
  let resolve!: (response: Response) => void
  const promise = new Promise<Response>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

beforeEach(async () => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true)
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    }
  )
  window.matchMedia = vi.fn().mockImplementation(() => ({
    matches: false,
    addEventListener() {},
    removeEventListener() {},
  }))
  notifyManager.setScheduler(queueMicrotask)
  setTokens({ access_token: "console-test", merchant: "merchant-one" })
  requests = []
  answer = async () => result("succeeded")
  previewAnswer = async () => Response.json(preview)
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      if (url.endsWith("config.json"))
        return Response.json({ api_base_url: "/v1", auth_base_url: "/auth" })
      if (url.endsWith("/preview")) return previewAnswer()
      if (url.endsWith("/change-tier")) {
        requests.push({
          key: new Headers(init?.headers).get("Idempotency-Key")!,
          price: JSON.parse(String(init?.body)).price_id,
        })
        return answer()
      }
      throw new Error(`Unexpected request: ${url}`)
    })
  )
  await loadBootstrap()
  client = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: Infinity },
      mutations: { retry: false },
    },
  })
  client.setQueryData(adminQueries.allProducts().queryKey, {
    total: 3,
    limit: 3,
    offset: 0,
    items: ["basic", "pro", "plus"].map((id, tier_rank) => ({
      id,
      key: id,
      description: "",
      archived: false,
      created_at: "2026-09-18T00:00:00Z",
      updated_at: "2026-09-18T00:00:00Z",
      display_name: id,
      tier_group: "plans",
      tier_rank,
    })),
  })
  client.setQueryData(adminQueries.allPrices().queryKey, {
    total: 3,
    limit: 3,
    offset: 0,
    items: ["basic", "pro", "plus"].map((id) => ({
      id: `price-${id}`,
      key: `price-${id}`,
      archived: false,
      created_at: "2026-09-18T00:00:00Z",
      updated_at: "2026-09-18T00:00:00Z",
      product_id: id,
      currency: "USD",
      unit_amount: "20000000",
      auto_renew: true,
    })),
  })
  const container = document.createElement("div")
  document.body.append(container)
  root = createRoot(container)
  await act(async () =>
    root.render(
      <QueryClientProvider client={client}>
        <ChangeTierDialog
          subscriptionId="sub-one"
          customerId="customer-one"
          productId="basic"
          priceId="price-basic"
          currency="USD"
          hasPendingReprice={false}
          rail="stripe"
          status="active"
        />
      </QueryClientProvider>
    )
  )
})

afterEach(async () => {
  await act(async () => root.unmount())
  client.clear()
  document.body.innerHTML = ""
  notifyManager.setScheduler((callback) => setTimeout(callback, 0))
  vi.unstubAllGlobals()
})

function button(label: string) {
  const found = [
    ...document.querySelectorAll<HTMLButtonElement>("button"),
  ].find((node) => node.textContent === label)
  expect(found, `button ${label}`).toBeDefined()
  expect(found!.disabled).toBe(false)
  return found!
}
async function click(label: string) {
  await act(async () => button(label).click())
}
async function select(plan = "pro") {
  await act(async () =>
    document.querySelector<HTMLButtonElement>('[role="combobox"]')!.click()
  )
  const option = [
    ...document.querySelectorAll<HTMLElement>('[role="option"]'),
  ].find((node) => node.textContent?.startsWith(`${plan} ·`))!
  expect(option).toBeDefined()
  await act(async () => option.click())
}
async function review(plan = "pro") {
  await select(plan)
  await click("Review change")
  button("Confirm upgrade")
}
async function openAndReview() {
  await click("Change tier")
  await review()
}

describe("the mounted tier-change dialog", () => {
  it("retains the same wire key across a lost response, dismissal, another preview and processing readback", async () => {
    await openAndReview()
    answer = async () => {
      throw new TypeError("Network response lost")
    }
    await click("Confirm upgrade")
    const original = requests[0].key
    expect(original).toMatch(/^[\da-f-]{36}$/)
    await click("Cancel")
    await click("Change tier")
    // Choosing another plan and returning must not erase an unresolved key.
    await select("plus")
    await review()
    answer = async () => result("processing")
    await click("Confirm upgrade")
    await click("Cancel")
    await click("Change tier")
    answer = async () => result("succeeded")
    await click("Check result")
    expect(requests).toEqual(
      Array.from({ length: 3 }, () => ({ key: original, price: "price-pro" }))
    )
  })

  it.each(["stripe_card_declined", "nmi_do_not_honor", "new_provider_refusal"])(
    "starts a fresh attempt after HTTP 402 (%s)",
    async (code) => {
      await openAndReview()
      answer = async () => decline(code)
      await click("Confirm upgrade")
      answer = async () => result("succeeded")
      await click("Confirm upgrade")
      expect(requests).toHaveLength(2)
      expect(requests[1].key).not.toBe(requests[0].key)
    }
  )

  it.each([
    [409, "tier_change_refused", false],
    [409, "tier_change_idempotency_conflict", false],
    [409, "tier_change_in_flight", true],
    [409, "unknown_conflict", true],
    [403, "permission_denied", true],
    [503, "provider_unavailable", true],
  ])(
    "keeps only an uncertain attempt on HTTP %i (%s)",
    async (status, code, keep) => {
      await openAndReview()
      answer = async () =>
        Response.json({ error: { code, message: "Not completed" } }, { status })
      await click("Confirm upgrade")
      answer = async () => result("succeeded")
      await click("Confirm upgrade")
      expect(requests).toHaveLength(2)
      expect(requests[1].key === requests[0].key).toBe(keep)
    }
  )

  it("does not let a dismissed request's late response clear or close the newer request", async () => {
    await openAndReview()
    const old = pendingResponse()
    answer = () => old.promise
    await click("Confirm upgrade")
    await click("Cancel")
    await click("Change tier")
    await review("plus")
    answer = async () => result("processing")
    await click("Confirm upgrade")
    const newerKey = requests[1].key
    await act(async () => old.resolve(result("succeeded")))
    button("Check result")
    answer = async () => result("succeeded")
    await click("Check result")
    expect(requests[2]).toEqual({ key: newerKey, price: "price-plus" })
  })

  it("ignores a preview that completes after its dialog was dismissed", async () => {
    await click("Change tier")
    await select()
    const old = pendingResponse()
    previewAnswer = () => old.promise
    await click("Review change")
    await click("Cancel")
    await click("Change tier")
    await select("plus")
    await act(async () => old.resolve(Response.json(preview)))
    button("Review change")
    expect(requests).toHaveLength(0)
  })
})
