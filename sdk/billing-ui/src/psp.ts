// OpenRails's browser-safe PSP projection (GET /config's payment). Which
// browser flow a PSP needs is decided here, never by the host.
import { z } from "zod"

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
   * New purchases and new cards use this PSP under the merchant's routing.
   * Other PSPs stay listed so existing cards and subscriptions keep working.
   */
  routed: z.boolean().nullish(),
  /**
   * `temporarily_unavailable`: the PSP could not be checked just now and is
   * listed without `config`; ask again after `retry_after` seconds.
   */
  status: z.string().nullish(),
  retry_after: z.number().int().nullish(),
})
export type PspConfig = z.infer<typeof pspConfigSchema>

/** PSPs that take new purchases and new cards. */
export const routedPsps = (psps: readonly PspConfig[]): PspConfig[] =>
  psps.filter((psp) => psp.routed !== false)

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
