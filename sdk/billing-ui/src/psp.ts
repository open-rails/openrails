// OpenRails's browser-safe PSP projection (GET /checkout-config, or the host's
// copy of it). Hosts pass it through unchanged; which browser flow a PSP needs
// is decided here, never by the host.
import { z } from "zod"

import type { PaymentMethod } from "./client/types"
import type { PaymentRailOption, SavedPaymentMethod } from "./types"

export const pspConfigSchema = z.object({
  psp_id: z.string().min(1),
  /** Checkout's `payment.rail` selector. */
  key: z.string().min(1),
  rail: z.string(),
  /** Who holds the card: "psp" or a third-party custodian. */
  custodian: z.string(),
  display_name: z.string(),
  /** `tokenize | elements | redirect | wallet` */
  flow: z.string(),
  config: z.record(z.string(), z.string()).nullish(),
  /**
   * New purchases and new cards use this PSP (the operator's checkout PSP).
   * Other PSPs stay listed so existing cards and subscriptions keep working.
   */
  checkout: z.boolean().nullish(),
})
export type PspConfig = z.infer<typeof pspConfigSchema>

/**
 * One way OpenRails can sell a price now (`ListCheckoutRailOptions`): the PSP
 * is armed and its rail supports the sale. OpenRails decides the browser
 * `driver` and `public_config`; an offer without a driver cannot run here.
 */
export interface CheckoutRailOffer {
  selector: string
  psp_id: string
  rail: string
  mode: string
  driver?: string | null
  public_config?: Record<string, string> | null
}

const drivers = new Set<string>([
  "collect_js",
  "stripe_elements",
  "redirect",
  "solana_pay",
])

/** The rails this UI can drive, exactly as OpenRails advertised them. */
export function checkoutRails(
  offers: readonly CheckoutRailOffer[]
): PaymentRailOption[] {
  return offers.flatMap((offer) => {
    if (!offer.driver || !drivers.has(offer.driver)) return []
    return [
      {
        id: offer.psp_id,
        rail: offer.rail,
        mode: offer.mode === "subscription" ? "subscription" : "one_off",
        driver: offer.driver as PaymentRailOption["driver"],
        psp_key: offer.selector,
        ...(offer.public_config ? { public_config: offer.public_config } : {}),
      },
    ]
  })
}

const cardDrivers = new Set<PaymentRailOption["driver"]>([
  "collect_js",
  "stripe_elements",
])

/** Whether a rail takes a card in the page (saved or new). */
export const isCardRail = (rail: PaymentRailOption): boolean =>
  cardDrivers.has(rail.driver)

/** PSPs that take new purchases and new cards. */
export const checkoutPsps = (psps: readonly PspConfig[]): PspConfig[] =>
  psps.filter((psp) => psp.checkout !== false)

/**
 * Saved cards the checkout can charge in place, for the given rails, most
 * recent first.
 */
export function savedMethodsFor(
  methods: readonly PaymentMethod[],
  rails: readonly PaymentRailOption[]
): SavedPaymentMethod[] {
  const recent = [...methods].sort((a, b) =>
    (b.created_at ?? "").localeCompare(a.created_at ?? "")
  )
  return recent.flatMap((method) => {
    const rail = rails.find(
      (item) => item.id === method.psp_id && isCardRail(item)
    )
    if (!rail || method.health?.active === false) return []
    return [
      {
        id: method.id,
        option_id: rail.id,
        rail: rail.rail,
        brand: method.card?.brand ?? undefined,
        last_four: method.card?.last4 ?? undefined,
        exp_month: method.card?.exp_month ?? undefined,
        exp_year: method.card?.exp_year ?? undefined,
      },
    ]
  })
}

export type CardSetupDriver = "collect_js" | "stripe_elements"

const usable = (value?: string) => !!value && !value.startsWith("preview_")

const stripeKey = (psp: PspConfig) =>
  psp.rail === "stripe" && !!psp.config?.publishable_key?.startsWith("pk_")

/** How a card is saved with this PSP in the page, or null when it cannot be. */
export function cardSetupDriver(psp: PspConfig): CardSetupDriver | null {
  if (psp.custodian !== "psp") return null
  if (
    psp.flow === "tokenize" &&
    usable(psp.config?.tokenization_key) &&
    !!psp.config?.tokenization_url
  )
    return "collect_js"
  if (stripeKey(psp)) return "stripe_elements"
  return null
}

export const canSavePaymentMethod = (psp: PspConfig): boolean =>
  cardSetupDriver(psp) !== null

/** Whether a pending payment on this PSP can be authenticated in the page. */
export const canAuthenticatePayment = (psp: PspConfig): boolean =>
  cardSetupDriver(psp) === "stripe_elements"

/** The PSP config behind a checkout rail, for card setup and authentication. */
export function railPsp(rail: PaymentRailOption): PspConfig {
  return {
    psp_id: rail.id,
    key: rail.psp_key ?? rail.rail,
    rail: rail.rail,
    custodian: "psp",
    display_name: "",
    flow: rail.driver === "stripe_elements" ? "elements" : "tokenize",
    config: rail.public_config ?? null,
  }
}
