// OpenRails's browser-safe PSP projection (GET /config's payment). Which
// browser flow a PSP needs is decided here, never by the host.
import { z } from "zod"

import type { PaymentMethod } from "./client/types"
import type { PaymentOption, SavedPaymentMethod } from "./types"

export const pspConfigSchema = z.object({
  psp_id: z.string().min(1),
  /** The PSP's key. */
  key: z.string().min(1),
  rail: z.string(),
  /** Who holds the card: "psp" or a third-party custodian. */
  custodian: z.string(),
  display_name: z.string(),
  /** `tokenize | card | elements | redirect | wallet` */
  flow: z.string(),
  config: z.record(z.string(), z.string()).nullish(),
  /**
   * New purchases and new cards use this PSP (the operator's checkout PSP).
   * Other PSPs stay listed so existing cards and subscriptions keep working.
   */
  checkout: z.boolean().nullish(),
  /**
   * `temporarily_unavailable`: the PSP could not be checked just now and is
   * listed without `config`; ask again after `retry_after` seconds.
   */
  status: z.string().nullish(),
  retry_after: z.number().int().nullish(),
})
export type PspConfig = z.infer<typeof pspConfigSchema>

const cardDrivers = new Set<PaymentOption["driver"]>([
  "collect_js",
  "card",
  "stripe_elements",
])

/** Whether a rail takes a card in the page (saved or new). */
export const isCardRail = (rail: PaymentOption): boolean =>
  cardDrivers.has(rail.driver)

/** PSPs that take new purchases and new cards. */
export const checkoutPsps = (psps: readonly PspConfig[]): PspConfig[] =>
  psps.filter((psp) => psp.checkout !== false)

const cardFlows = new Set(["tokenize", "card", "elements"])

/**
 * Seconds until a card PSP listed as temporarily unavailable is worth asking
 * for again; null when none is.
 */
export function cardRetryAfter(psps: readonly PspConfig[]): number | null {
  const waits = psps
    .filter(
      (psp) =>
        !!psp.status && psp.custodian === "psp" && cardFlows.has(psp.flow)
    )
    .map((psp) => psp.retry_after ?? 30)
  return waits.length > 0 ? Math.min(...waits) : null
}

/** Saved cards the checkout can charge in place, for the given rails, newest first. */
export function savedMethodsFor(
  methods: readonly PaymentMethod[],
  rails: readonly PaymentOption[]
): SavedPaymentMethod[] {
  const recent = [...methods].sort((a, b) =>
    (b.created_at ?? "").localeCompare(a.created_at ?? "")
  )
  return recent.flatMap((method) => {
    const rail = rails.find(
      (item) => (item.psp_id ?? item.id) === method.psp_id && isCardRail(item)
    )
    if (!rail || method.health?.active === false) return []
    return [
      {
        id: method.id,
        option_id: rail.id,
        rail: rail.rail,
        card: method.card ?? null,
      },
    ]
  })
}

export type CardSetupDriver = "collect_js" | "card" | "stripe_elements"

const usable = (value?: string) => !!value && !value.startsWith("preview_")

const stripeKey = (psp: PspConfig) =>
  psp.rail === "stripe" && !!psp.config?.publishable_key?.startsWith("pk_")

/** How a card is saved with this PSP in the page, or null when it cannot be. */
export function cardSetupDriver(psp: PspConfig): CardSetupDriver | null {
  if (psp.custodian !== "psp" || psp.status) return null
  // The PSP takes cards on OpenRails itself (card_entry: server).
  if (psp.flow === "card" && psp.rail === "nmi") return "card"
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
export function railPsp(rail: PaymentOption): PspConfig {
  return {
    psp_id: rail.psp_id ?? rail.id,
    key: rail.psp_key ?? rail.rail,
    rail: rail.rail,
    custodian: "psp",
    display_name: "",
    flow:
      rail.driver === "stripe_elements"
        ? "elements"
        : rail.driver === "card"
          ? "card"
          : "tokenize",
    config: rail.public_config ?? null,
  }
}
