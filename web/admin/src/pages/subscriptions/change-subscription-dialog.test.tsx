// @vitest-environment jsdom
// The subscription-change key lifetime, proved on the mounted dialog against the real
// API client (#513): one reviewed change is one durable operation, and only a
// definitive refusal may start a new one.
import { QueryClientProvider } from "@tanstack/react-query"
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from "vitest"

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
import { ChangeSubscriptionDialog } from "./change-subscription-dialog"
import {
  adminSubscriptionChangeBlockReason,
  subscriptionChangeOptionLabel,
  subscriptionChangeOptions,
} from "./subscription-change-options"

const preview = {
  price_id: "price-pro", quantity: null, rail: "stripe", currency: "USD",
  amount_due_now: "0", next_charge_amount: "20000000",
  effective: "period_end", is_estimate: false,
}
const result = (status: string) =>
  Response.json(
    { price_id: "price-pro", quantity: null, rail: "stripe", status,
      effective: "period_end", transaction_id: null, amount_due_now: "0",
      next_charge_amount: "20000000" },
    { status: status === "processing" ? 202 : 200 }
  )
const refusal = (status: number, code: string) =>
  Response.json({ error: { code, message: "Not completed" } }, { status })

function pending() {
  let resolve!: (response: Response) => void
  return { promise: new Promise<Response>((done) => (resolve = done)), resolve }
}

// What reached the server: the key each attempt carried and the plan it named.
let sent: { key: string; price?: string; quantity?: number }[]
let answer: () => Promise<Response>
let previewAnswer: () => Promise<Response>
// A per-seat subscription holds 2 seats of prices bounded 1–10.
let perSeat = false

beforeEach(async () => {
  browserEnvironment()
  sent = []
  answer = async () => result("succeeded")
  previewAnswer = async () => Response.json(preview)
  const routes: Record<string, Reply> = {
    "POST /admin/subscriptions/sub-one/change/preview": () => previewAnswer(),
    "POST /admin/subscriptions/sub-one/change": (request) => {
      const body = request.body as { price_id?: string; quantity?: number }
      sent.push({
        key: request.headers.get("Idempotency-Key")!,
        ...(body.price_id ? { price: body.price_id } : {}),
        ...(body.quantity ? { quantity: body.quantity } : {}),
      })
      return answer()
    },
  }
  await server(routes)
  selectMerchant("merchant-one")
  await mountDialog()
})
afterEach(unmount)

async function mountDialog() {
  const queryClient = client({ staleTime: Infinity })
  const plans = ["basic", "pro", "plus"]
  const quantity = perSeat ? { min: 1, max: 10 } : null
  const page = <T,>(data: T[]) => ({ data, next_cursor: null })
  queryClient.setQueryData(
    adminQueries.allProducts().queryKey,
    page(plans.map((id, rank) => aProduct(id, rank)))
  )
  queryClient.setQueryData(
    adminQueries.allPrices().queryKey,
    page(plans.map((id) => aPrice(`price-${id}`, id, { quantity })))
  )
  await mount(
    <QueryClientProvider client={queryClient}>
      <ChangeSubscriptionDialog
        subscriptionId="sub-one" customerId="customer-one" productId="basic"
        priceId="price-basic" quantity={perSeat ? 2 : null} currency="USD"
        collectionPolicy="engine" scheduledChange={null}
        rail="stripe" status="active"
      />
    </QueryClientProvider>
  )
}

async function review(plan = "pro") {
  await choose(`${plan} ·`)
  await click("Review change")
  button("Confirm change")
}
async function openAndReview() {
  await click("Change subscription")
  await review()
}

async function typeInto(selector: string, value: string) {
  await act(async () => {
    const input = document.querySelector<HTMLInputElement>(selector)!
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, value)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  })
}
const typeSeats = (value: string) => typeInto("#subscription-seats", value)

describe("the mounted subscription-change dialog", () => {
  it("retains the same wire key across a lost response, dismissal, another preview and processing readback", async () => {
    await openAndReview()
    answer = async () => {
      throw new TypeError("Network response lost")
    }
    await click("Confirm change")
    const original = sent[0].key
    expect(original).toMatch(/^[\da-f-]{36}$/)
    await click("Cancel")
    await click("Change subscription")
    // Choosing another plan and returning must not erase an unresolved key.
    await choose("plus ·")
    await review()
    answer = async () => result("processing")
    await click("Confirm change")
    await click("Cancel")
    await click("Change subscription")
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
      await click("Confirm change")
      answer = async () => result("succeeded")
      await click("Confirm change")
      expect(sent).toHaveLength(2)
      expect(sent[1].key).not.toBe(sent[0].key)
    }
  )

  it.each([
    [409, "subscription_change_refused", false],
    [409, "subscription_change_idempotency_conflict", false],
    [409, "subscription_change_in_flight", true],
    [409, "unknown_conflict", true],
    [403, "permission_denied", true],
    [503, "provider_unavailable", true],
  ])("keeps only an uncertain attempt on HTTP %i (%s)", async (status, code, keep) => {
    await openAndReview()
    answer = async () => refusal(status, code)
    await click("Confirm change")
    answer = async () => result("succeeded")
    await click("Confirm change")
    expect(sent).toHaveLength(2)
    expect(sent[1].key === sent[0].key).toBe(keep)
  })

  it("does not let a dismissed request's late response clear or close the newer request", async () => {
    await openAndReview()
    const old = pending()
    answer = () => old.promise
    await click("Confirm change")
    await click("Cancel")
    await click("Change subscription")
    await review("plus")
    answer = async () => result("processing")
    await click("Confirm change")
    const newerKey = sent[1].key
    await act(async () => old.resolve(result("succeeded")))
    button("Check result")
    answer = async () => result("succeeded")
    await click("Check result")
    expect(sent[2]).toEqual({ key: newerKey, price: "price-plus" })
  })

  it("ignores a preview that completes after its dialog was dismissed", async () => {
    await click("Change subscription")
    await choose("pro ·")
    const old = pending()
    previewAnswer = () => old.promise
    await click("Review change")
    await click("Cancel")
    await click("Change subscription")
    await choose("plus ·")
    await act(async () => old.resolve(Response.json(preview)))
    button("Review change")
    expect(sent).toHaveLength(0)
  })
})

describe("the mounted dialog on a per-seat subscription", () => {
  beforeAll(() => {
    perSeat = true
  })
  afterAll(() => {
    perSeat = false
  })

  it("changes only the seats", async () => {
    await click("Change subscription")
    await choose("basic · current")
    // The current seats are no change to review.
    const reviewButton = () =>
      [...document.querySelectorAll("button")].find((node) => node.textContent === "Review change")!
    expect(reviewButton().disabled).toBe(true)
    await typeSeats("11")
    expect(reviewButton().disabled).toBe(true)
    await typeSeats("5")
    await click("Review change")
    await click("Confirm change")
    expect(sent).toEqual([{ key: expect.any(String), price: "price-basic", quantity: 5 }])
  })

  it("keeps the subscription's seats on another per-seat plan", async () => {
    await click("Change subscription")
    await review()
    await click("Confirm change")
    expect(sent).toEqual([{ key: expect.any(String), price: "price-pro", quantity: 2 }])
  })
})

// Which changes the console offers at all: a pure invariant the dialog
// cannot prove, because an option it never lists cannot be clicked.
describe("subscription change options", () => {
  it("offers only live recurring prices in the current group and currency", () => {
    const current = aProduct("standard", 2)
    expect(
      subscriptionChangeOptions({
        currentProduct: current,
        currentCurrency: "USD",
        products: [current, aProduct("basic", 1), aProduct("pro", 3),
          aProduct("other", 4, { tier_group: "storage" }),
          aProduct("archived", 5, { archived: true })],
        prices: [
          aPrice("basic-usd", "basic", { currency: "usd" }),
          aPrice("pro-usd", "pro"),
          aPrice("pro-eur", "pro", { currency: "eur" }),
          aPrice("pro-once", "pro", { billing_interval_hours: null }),
          aPrice("other-usd", "other"),
          aPrice("archived-product", "archived"),
          aPrice("archived-price", "pro", { archived: true }),
        ],
      }).map(({ direction, price }) => [direction, price.id])
    ).toEqual([["downgrade", "basic-usd"], ["upgrade", "pro-usd"]])
  })

  it("offers the current plan when it is per-seat or a change is pending, and only it while a migration is", () => {
    const current = aProduct("standard", 2)
    const seated = aPrice("standard-seats", "standard", { quantity: { min: 1, max: 5 } })
    const options = (source?: "change" | "migration") =>
      subscriptionChangeOptions({
        currentProduct: current, currentPrice: seated, currentCurrency: "USD",
        products: [current, aProduct("pro", 3)],
        prices: [seated, aPrice("pro-usd", "pro")],
        scheduledChange: source ? { source } : null,
      }).map(({ direction, price }) => [direction, price.id])
    expect(options()).toEqual([["current", "standard-seats"], ["upgrade", "pro-usd"]])
    expect(options("migration")).toEqual([["current", "standard-seats"]])
    const flat = aPrice("standard-flat", "standard")
    expect(
      subscriptionChangeOptions({
        currentProduct: current, currentPrice: flat, currentCurrency: "USD",
        products: [current], prices: [flat], scheduledChange: { source: "change" },
      }).map(({ direction }) => direction)
    ).toEqual(["current"])
  })

  it("fails closed when the current product has no tier group", () => {
    expect(
      subscriptionChangeOptions({
        currentProduct: aProduct("standalone", 0, { tier_group: undefined }),
        currentCurrency: "usd",
        products: [aProduct("other", 1, { tier_group: undefined })],
        prices: [aPrice("other-usd", "other")],
      })
    ).toEqual([])
  })

  it("distinguishes recurring variants by cadence and key", () => {
    expect(
      subscriptionChangeOptionLabel({
        direction: "upgrade",
        product: aProduct("pro", 3),
        price: aPrice("pro-monthly", "pro", { billing_interval_hours: 720, access_duration_hours: 72 }),
      })
    ).toBe("pro · upgrade · $20.00 every 30 days · pro-monthly")
  })

  it.each([
    [{}, undefined],
    [{ status: "canceled" as const }, "Only active or past-due subscriptions can change"],
    [{ scheduledChange: { source: "change" as const } }, "Its provider already bills a scheduled change"],
    [{ scheduledChange: { source: "migration" as const }, collectionPolicy: "engine" }, undefined],
    [{ rail: "ccbill" }, "CCBill changes require customer self-service"],
    [{ rail: "solana" }, "Solana changes require the customer's wallet signature"],
  ])("blocks the admin workflow it cannot complete: %o", (override, reason) => {
    expect(
      adminSubscriptionChangeBlockReason({
        rail: "nmi", status: "active", scheduledChange: null, ...override,
      })
    ).toBe(reason)
  })
})
