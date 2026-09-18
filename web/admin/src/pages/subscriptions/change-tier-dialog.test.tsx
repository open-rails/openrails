// @vitest-environment jsdom
// The tier-change key lifetime, proved on the mounted dialog against the real
// API client (#513): one reviewed change is one durable operation, and only a
// definitive refusal may start a new one.
import { QueryClientProvider } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it } from "vitest"

import { adminQueries } from "@/lib/queries"
import {
  aPrice,
  aProduct,
  client,
  selectMerchant,
  server,
  type Reply,
} from "@/test/harness"
import { act, browserEnvironment, button, choose, click, mount, unmount } from "@/test/mount"
import { ChangeTierDialog } from "./change-tier-dialog"
import {
  adminTierChangeBlockReason,
  tierChangeOptionLabel,
  tierChangeOptions,
} from "./tier-change-options"

const preview = {
  object: "tier_change_preview", action: "upgrade", price_id: "price-pro",
  rail: "stripe", currency: "USD", amount_due_now: "12000000",
  next_charge_amount: "20000000", effective: "now", is_estimate: false,
}
const result = (status: string) =>
  Response.json(
    { object: "tier_change", mode: "tier_change", action: "upgrade",
      price_id: "price-pro", payment: { rail: "stripe" }, status },
    { status: status === "processing" ? 202 : 200 }
  )
const refusal = (status: number, code: string) =>
  Response.json({ error: { code, message: "Not completed" } }, { status })

function pending() {
  let resolve!: (response: Response) => void
  return { promise: new Promise<Response>((done) => (resolve = done)), resolve }
}

// What reached the server: the key each attempt carried and the tier it named.
let sent: { key: string; price: string }[]
let answer: () => Promise<Response>
let previewAnswer: () => Promise<Response>

beforeEach(async () => {
  browserEnvironment()
  sent = []
  answer = async () => result("succeeded")
  previewAnswer = async () => Response.json(preview)
  const routes: Record<string, Reply> = {
    "POST /merchant/subscriptions/sub-one/change-tier/preview": () => previewAnswer(),
    "POST /merchant/subscriptions/sub-one/change-tier": (request) => {
      sent.push({
        key: request.headers.get("Idempotency-Key")!,
        price: (request.body as { price_id: string }).price_id,
      })
      return answer()
    },
  }
  await server(routes)
  selectMerchant("merchant-one")
  const queryClient = client({ staleTime: Infinity })
  const plans = ["basic", "pro", "plus"]
  const page = <T,>(items: T[]) => ({ total: 3, limit: 3, offset: 0, items })
  queryClient.setQueryData(
    adminQueries.allProducts().queryKey,
    page(plans.map((id, rank) => aProduct(id, rank)))
  )
  queryClient.setQueryData(
    adminQueries.allPrices().queryKey,
    page(plans.map((id) => aPrice(`price-${id}`, id)))
  )
  await mount(
    <QueryClientProvider client={queryClient}>
      <ChangeTierDialog
        subscriptionId="sub-one" customerId="customer-one" productId="basic"
        priceId="price-basic" currency="USD" hasPendingReprice={false}
        rail="stripe" status="active"
      />
    </QueryClientProvider>
  )
})
afterEach(unmount)

async function review(plan = "pro") {
  await choose(`${plan} ·`)
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
    const original = sent[0].key
    expect(original).toMatch(/^[\da-f-]{36}$/)
    await click("Cancel")
    await click("Change tier")
    // Choosing another plan and returning must not erase an unresolved key.
    await choose("plus ·")
    await review()
    answer = async () => result("processing")
    await click("Confirm upgrade")
    await click("Cancel")
    await click("Change tier")
    answer = async () => result("succeeded")
    await click("Check result")
    expect(sent).toEqual(
      Array.from({ length: 3 }, () => ({ key: original, price: "price-pro" }))
    )
  })

  it.each(["stripe_card_declined", "nmi_do_not_honor", "new_provider_refusal"])(
    "starts a fresh attempt after HTTP 402 (%s)",
    async (code) => {
      await openAndReview()
      answer = async () => refusal(402, code)
      await click("Confirm upgrade")
      answer = async () => result("succeeded")
      await click("Confirm upgrade")
      expect(sent).toHaveLength(2)
      expect(sent[1].key).not.toBe(sent[0].key)
    }
  )

  it.each([
    [409, "tier_change_refused", false],
    [409, "tier_change_idempotency_conflict", false],
    [409, "tier_change_in_flight", true],
    [409, "unknown_conflict", true],
    [403, "permission_denied", true],
    [503, "provider_unavailable", true],
  ])("keeps only an uncertain attempt on HTTP %i (%s)", async (status, code, keep) => {
    await openAndReview()
    answer = async () => refusal(status, code)
    await click("Confirm upgrade")
    answer = async () => result("succeeded")
    await click("Confirm upgrade")
    expect(sent).toHaveLength(2)
    expect(sent[1].key === sent[0].key).toBe(keep)
  })

  it("does not let a dismissed request's late response clear or close the newer request", async () => {
    await openAndReview()
    const old = pending()
    answer = () => old.promise
    await click("Confirm upgrade")
    await click("Cancel")
    await click("Change tier")
    await review("plus")
    answer = async () => result("processing")
    await click("Confirm upgrade")
    const newerKey = sent[1].key
    await act(async () => old.resolve(result("succeeded")))
    button("Check result")
    answer = async () => result("succeeded")
    await click("Check result")
    expect(sent[2]).toEqual({ key: newerKey, price: "price-plus" })
  })

  it("ignores a preview that completes after its dialog was dismissed", async () => {
    await click("Change tier")
    await choose("pro ·")
    const old = pending()
    previewAnswer = () => old.promise
    await click("Review change")
    await click("Cancel")
    await click("Change tier")
    await choose("plus ·")
    await act(async () => old.resolve(Response.json(preview)))
    button("Review change")
    expect(sent).toHaveLength(0)
  })
})

// Which tier changes the console offers at all: a pure invariant the dialog
// cannot prove, because an option it never lists cannot be clicked.
describe("tier change options", () => {
  it("offers only live recurring prices in the current group and currency", () => {
    const current = aProduct("standard", 2)
    expect(
      tierChangeOptions({
        currentProduct: current,
        currentCurrency: "USD",
        products: [current, aProduct("basic", 1), aProduct("pro", 3),
          aProduct("other", 4, { tier_group: "storage" }),
          aProduct("archived", 5, { archived: true })],
        prices: [
          aPrice("basic-usd", "basic", { currency: "usd" }),
          aPrice("pro-usd", "pro"),
          aPrice("pro-eur", "pro", { currency: "eur" }),
          aPrice("pro-once", "pro", { auto_renew: false }),
          aPrice("other-usd", "other"),
          aPrice("archived-product", "archived"),
          aPrice("archived-price", "pro", { archived: true }),
        ],
      }).map(({ direction, price }) => [direction, price.id])
    ).toEqual([["downgrade", "basic-usd"], ["upgrade", "pro-usd"]])
  })

  it("fails closed when the current product has no tier group", () => {
    expect(
      tierChangeOptions({
        currentProduct: aProduct("standalone", 0, { tier_group: undefined }),
        currentCurrency: "usd",
        products: [aProduct("other", 1, { tier_group: undefined })],
        prices: [aPrice("other-usd", "other")],
      })
    ).toEqual([])
  })

  it("distinguishes recurring variants by cadence and key", () => {
    expect(
      tierChangeOptionLabel({
        direction: "upgrade",
        product: aProduct("pro", 3),
        price: aPrice("pro-monthly", "pro", { access_duration_hours: 720 }),
      })
    ).toBe("pro · upgrade · $20.00 every 1 month · pro-monthly")
  })

  it.each([
    [{}, undefined],
    [{ status: "cancelled" as const }, "Only active or past-due subscriptions can change tier"],
    [{ scheduledPriceId: "price-next" }, "A tier change is already scheduled"],
    [{ hasPendingReprice: true }, "A price change is already scheduled"],
    [{ rail: "ccbill" }, "CCBill tier changes require customer self-service"],
    [{ rail: "solana" }, "Solana tier changes require the customer's wallet signature"],
  ])("blocks the admin workflow it cannot complete: %o", (override, reason) => {
    expect(
      adminTierChangeBlockReason({
        rail: "nmi", status: "active", scheduledPriceId: null,
        hasPendingReprice: false, ...override,
      })
    ).toBe(reason)
  })
})
