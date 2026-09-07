// Fixture sources: drive every UI state without a backend or gateway. Used
// by previews and tests; the "preview_" tokenization key makes CardFields
// render inert placeholders instead of live Collect.js iframes.
import type { CheckoutSource } from "./source"
import type { CheckoutSession, PayResult } from "./types"

export function fixtureSession(
  overrides?: Partial<CheckoutSession>
): CheckoutSession {
  return {
    id: "ocs_preview",
    status: "created",
    merchant: { display_name: "Acme Demo" },
    plan: {
      display_name: "Premium Membership",
      unit_amount_micros: 99_000_000,
      currency: "usd",
      period_hours: 720,
      automatically_renews: true,
    },
    tax_micros: 0,
    rails: [
      {
        id: "option_1",
        rail: "nmi",
        mode: "subscription",
        driver: "collect_js",
        public_config: {
          tokenization_key: "preview_tokenization_key",
          tokenization_url: "preview://collect",
        },
      },
      {
        id: "option_2",
        rail: "stripe",
        mode: "subscription",
        driver: "redirect",
      },
      {
        id: "option_3",
        rail: "ccbill",
        mode: "subscription",
        driver: "redirect",
      },
      {
        id: "option_4",
        rail: "solana",
        mode: "one_off",
        driver: "solana_pay",
        public_config: { token_symbol: "USDC", token_name: "USD Coin" },
      },
    ],
    saved_methods: [
      {
        id: "pm_preview_visa",
        option_id: "option_1",
        rail: "nmi",
        brand: "visa",
        last_four: "4242",
        exp_month: 12,
        exp_year: 2030,
      },
      {
        id: "pm_preview_mc",
        option_id: "option_1",
        rail: "nmi",
        brand: "mastercard",
        last_four: "5454",
        exp_month: 3,
        exp_year: 2029,
      },
    ],
    expires_at: new Date(Date.now() + 30 * 60 * 1000).toISOString(),
    ...overrides,
  }
}

export function createFixtureSource(options?: {
  session?: Partial<CheckoutSession>
  payResult?: PayResult
  payDelayMs?: number
  loadDelayMs?: number
}): CheckoutSource {
  return {
    async getSession() {
      if (options?.loadDelayMs) {
        await new Promise((resolve) => setTimeout(resolve, options.loadDelayMs))
      }
      return fixtureSession(options?.session)
    },
    async pay() {
      await new Promise((resolve) =>
        setTimeout(resolve, options?.payDelayMs ?? 1200)
      )
      return options?.payResult ?? { status: "succeeded" }
    },
  }
}
