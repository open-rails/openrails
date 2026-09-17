import { describe, expect, it } from "vitest"
import { amountFromInput, formatUnits } from "@/lib/format"

// Canonical Go wire fixtures (testdata/wire, pinned by wire_fixtures_test.go).
const fixtures = import.meta.glob<string>(
  "../../../../../testdata/wire/*.json",
  {
    query: "?raw",
    import: "default",
    eager: true,
  }
)

function fixture(name: string): string {
  const entry = Object.entries(fixtures).find(([path]) =>
    path.endsWith(`/${name}`)
  )
  if (!entry) throw new Error(`missing wire fixture ${name}`)
  return entry[1]
}

const maxInt64 = (1n << 63n) - 1n
const minInt64 = -(1n << 63n)

describe("canonical wire fixtures in the browser", () => {
  it("contain only JSON numbers JavaScript represents exactly", () => {
    expect(Object.keys(fixtures).length).toBeGreaterThanOrEqual(6)
    for (const [path, raw] of Object.entries(fixtures)) {
      const numbers =
        raw.replace(/"(?:[^"\\]|\\.)*"/g, '""').match(/-?\d[\d.eE+-]*/g) ?? []
      for (const token of numbers) {
        expect(
          Number.isSafeInteger(Number(token)) &&
            String(Number(token)) === token,
          `${path}: ${token}`
        ).toBe(true)
      }
      expect(() => JSON.parse(raw)).not.toThrow()
    }
  })

  it("round-trip int64 boundary money without Number conversion", () => {
    const page = JSON.parse(fixture("page_credit_transactions.json"))
    expect(page.object).toBe("list")
    expect(BigInt(page.data[0].amount)).toBe(maxInt64)
    expect(BigInt(page.data[1].balance_after)).toBe(minInt64)
    expect(page.data[0].authorized).toBeNull()
    expect(page.data[0].created_at).toBe("2026-09-16T00:00:00.123456789Z")
    expect(amountFromInput("9223372036854.775807", 6)).toBe(page.data[0].amount)
    expect(formatUnits(page.data[1].amount, "JPY", 0).replace(/\D/g, "")).toBe(
      minInt64.toString().slice(1)
    )

    const settings = JSON.parse(fixture("merchant_settings.json"))
    expect(BigInt(settings.billing_policies[1].spend_windows[0].limit)).toBe(
      maxInt64
    )
    expect(settings.billing_policies[1].spend_windows[0].window_seconds).toBe(
      2592000
    )
  })

  it("keep error details and empty lists structurally stable", () => {
    const envelope = JSON.parse(fixture("error_envelope.json"))
    expect(envelope.error.code).toBe("idempotency_key_reused")
    expect(envelope.error.param).toBe("amount")
    expect(BigInt(envelope.error.metadata.committed_amount)).toBe(maxInt64)
    expect(envelope.error.metadata.detail).toBeNull()

    const empty = JSON.parse(fixture("page_empty.json"))
    expect(empty.data).toEqual([])
    expect(empty.has_more).toBe(false)
  })
})

describe("typed ids on the wire", () => {
  const prefixed = (prefix: string) =>
    new RegExp(
      `^${prefix}[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
    )

  it("spell every prefixed kind with its prefix and customers as plain UUIDs", () => {
    const sub = JSON.parse(fixture("subscription.json"))
    expect(sub.id).toMatch(prefixed("sub_"))
    expect(sub.customer_id).toMatch(prefixed(""))
    expect(sub.product_id).toMatch(prefixed("prod_"))
    expect(sub.price_id).toMatch(prefixed("price_"))
    expect(sub.scheduled_price_id).toMatch(prefixed("price_"))
    expect(sub.payment_method_id).toMatch(prefixed("pm_"))
    expect(sub.payments[0].id).toMatch(prefixed("pay_"))
    expect(sub.price.id).toBe(sub.price_id)
    expect(sub.product.id).toBe(sub.product_id)

    const price = JSON.parse(fixture("catalog_price.json"))
    expect(price.id).toBe(sub.price_id)
    expect(price.product_id).toBe(sub.product_id)

    const session = JSON.parse(fixture("checkout_session.json"))
    expect(session.id).toMatch(prefixed("cs_"))
    expect(session.subscription_id).toBe(sub.id)
    expect(session.payment_id).toBe(sub.payments[0].id)

    const payment = JSON.parse(fixture("payment.json"))
    expect(payment.id).toBe(sub.payments[0].id)
    expect(payment.customer_id).toBe(sub.customer_id)
    expect(payment.subscription_id).toBe(sub.id)
    expect(payment.price.product).toBe(sub.product_id)

    // The self routes serve the same subscription shape: the ids it lists
    // are the ids its action routes take, and its access grant names them.
    expect(sub.scheduled_price.id).toBe(sub.scheduled_price_id)
    expect(sub.access.subscription_id).toBe(sub.id)
    expect(BigInt(sub.price.unit_amount)).toBe(maxInt64)
    const status = JSON.parse(fixture("billing_status.json"))
    expect(status.subscription.id).toBe(sub.id)
    expect(status.access.subscription_id).toBe(sub.id)
    expect(status.entitlements[0].source_id).toBe(sub.id)

    const hosted = JSON.parse(fixture("hosted_checkout_session.json"))
    expect(hosted.saved_methods[0].id).toBe(sub.payment_method_id)
    expect(hosted.payment_id).toBe(sub.payments[0].id)
    expect(hosted.subscription_id).toBe(sub.id)

    const notification = JSON.parse(fixture("notification.json"))
    expect(notification.customer_id).toBe(sub.customer_id)
    expect(notification.data.subscription_id).toBe(sub.id)
    expect(notification.data.from_price_id).toBe(sub.price_id)
    expect(BigInt(notification.data.old_amount)).toBe(maxInt64)
    expect(BigInt(notification.data.new_amount)).toBe(minInt64)
  })
})
