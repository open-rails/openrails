// Customer-facing rail vocabulary. Unknown rails are not rendered — a rail
// the UI cannot execute must not be offered.
import type { PaymentRailOption } from "#orck/types"

export interface RailMeta {
  label: string
  hint?: string
}

export const RAIL_META: Record<string, RailMeta> = {
  nmi: { label: "Card" },
  stripe: { label: "Stripe", hint: "Opens Stripe" },
  ccbill: { label: "CCBill", hint: "Opens CCBill" },
  solana: { label: "Crypto", hint: "USDC on Solana" },
}

export function supportedOptions(
  options: PaymentRailOption[]
): PaymentRailOption[] {
  return options.filter((option) => {
    if (RAIL_META[option.rail] === undefined) return false
    if (option.driver === "collect_js") return option.rail === "nmi"
    if (option.driver === "redirect")
      return option.rail === "stripe" || option.rail === "ccbill"
    return option.driver === "solana_pay" && option.rail === "solana"
  })
}
