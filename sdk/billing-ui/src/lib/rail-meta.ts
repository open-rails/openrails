// Customer-facing rail vocabulary. Unknown rails are not rendered — a rail
// the UI cannot execute must not be offered.
import type { PaymentRailOption } from "#orck/types"

export interface RailMeta {
  label: string
  hint?: string
}

// A card taken in the page reads "Card" whichever PSP charges it.
export function railMeta(option: PaymentRailOption): RailMeta {
  if (option.driver === "collect_js" || option.driver === "stripe_elements")
    return { label: "Card" }
  return RAIL_META[option.rail]
}

export const RAIL_META: Record<string, RailMeta> = {
  nmi: { label: "Card" },
  stripe: { label: "Stripe", hint: "Opens Stripe" },
  ccbill: { label: "CCBill", hint: "Opens CCBill" },
  // The Solana hint is not static: it names the token the host bound to the
  // option (see solanaToken), so the buyer sees the mint they will actually pay
  // with, on the network the host is actually on.
  solana: { label: "Crypto" },
}

// The Solana token the host bound to an option, read from its public_config:
// - token_symbol — required; the SPL mint the price is bound to (USDC, USD1…).
// - token_name   — optional buyer-facing name ("USD Coin").
// - network      — optional Solana cluster ("mainnet-beta", "devnet",
//                  "testnet"); anything but mainnet-beta is named in the UI so a
//                  test-network payment is never mistaken for a real one.
// The package never assumes a token: an option without token_symbol is not
// offered (supportedOptions) — a rail the UI cannot execute must not be shown.
export interface SolanaToken {
  symbol: string
  name?: string
  network?: string
}

export function solanaToken(
  option: PaymentRailOption
): SolanaToken | undefined {
  const symbol = option.public_config?.token_symbol?.trim().toUpperCase()
  if (!symbol) return undefined
  const name = option.public_config?.token_name?.trim()
  const network = option.public_config?.network?.trim().toLowerCase()
  return {
    symbol,
    name: name || undefined,
    network: network || undefined,
  }
}

export function solanaHint(token: SolanaToken): string {
  const label =
    token.name && token.name.toUpperCase() !== token.symbol
      ? `${token.name} (${token.symbol})`
      : token.symbol
  const chain =
    token.network && token.network !== "mainnet-beta"
      ? `Solana ${token.network}`
      : "Solana"
  return `${label} on ${chain}`
}

export function supportedOptions(
  options: PaymentRailOption[]
): PaymentRailOption[] {
  return options.filter((option) => {
    if (RAIL_META[option.rail] === undefined) return false
    if (option.driver === "collect_js") return option.rail === "nmi"
    if (option.driver === "stripe_elements") return option.rail === "stripe"
    if (option.driver === "redirect")
      return option.rail === "stripe" || option.rail === "ccbill"
    return (
      option.driver === "solana_pay" &&
      option.rail === "solana" &&
      solanaToken(option) !== undefined
    )
  })
}
