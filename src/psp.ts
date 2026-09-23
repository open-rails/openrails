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
  /** `tokenize | redirect | wallet` */
  flow: z.string(),
  config: z.record(z.string(), z.string()).nullish(),
})
export type PspConfig = z.infer<typeof pspConfigSchema>

/** One checkout rail OpenRails offers for a price (`ListCheckoutRailOptions`). */
export interface CheckoutRailOffer {
  psp_id: string
  rail: string
  mode: string
}

const usable = (value?: string) => !!value && !value.startsWith("preview_")

function checkoutDriver(psp: PspConfig): PaymentRailOption["driver"] | null {
  switch (psp.flow) {
    case "redirect":
      return "redirect"
    case "wallet":
      return "solana_pay"
    case "tokenize":
      return psp.custodian === "psp" &&
        usable(psp.config?.tokenization_key) &&
        !!psp.config?.tokenization_url
        ? "collect_js"
        : null
    default:
      return null
  }
}

/** The rails this UI can drive for an offer, keyed by PSP id. */
export function checkoutRails(
  offers: readonly CheckoutRailOffer[],
  psps: readonly PspConfig[]
): PaymentRailOption[] {
  return offers.flatMap((offer) => {
    const psp = psps.find((item) => item.psp_id === offer.psp_id)
    const driver = psp && checkoutDriver(psp)
    if (!psp || !driver) return []
    return [
      {
        id: psp.psp_id,
        rail: psp.rail,
        mode: offer.mode === "subscription" ? "subscription" : "one_off",
        driver,
        ...(psp.config ? { public_config: psp.config } : {}),
      },
    ]
  })
}

/** Saved cards the checkout can charge in place, for the given rails. */
export function savedMethodsFor(
  methods: readonly PaymentMethod[],
  rails: readonly PaymentRailOption[]
): SavedPaymentMethod[] {
  return methods.flatMap((method) => {
    const rail = rails.find(
      (item) => item.id === method.psp_id && item.driver === "collect_js"
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

/** How a card is saved with this PSP in the page, or null when it cannot be. */
export function cardSetupDriver(psp: PspConfig): CardSetupDriver | null {
  if (psp.custodian !== "psp") return null
  if (checkoutDriver(psp) === "collect_js") return "collect_js"
  if (psp.rail === "stripe" && psp.config?.publishable_key?.startsWith("pk_"))
    return "stripe_elements"
  return null
}

export const canSavePaymentMethod = (psp: PspConfig): boolean =>
  cardSetupDriver(psp) !== null

/** Whether a pending payment on this PSP can be authenticated in the page. */
export const canAuthenticatePayment = (psp: PspConfig): boolean =>
  cardSetupDriver(psp) === "stripe_elements"
