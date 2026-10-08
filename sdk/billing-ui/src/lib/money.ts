// Exact money. Every amount the host serves is a signed int64 decimal string
// of the plan currency's native unit; plan.unit_decimals is that currency's
// registered scale (OpenRails' currency registry, GET /v1/currencies). Nothing
// here converts an amount to a JS number: values are scaled with BigInt and
// only the exact major-unit decimal is handed to Intl.

// Amount is an int64 decimal string, e.g. "99000000" (99 USD at 6 decimals).
export type Amount = string

const INT64_MIN = -(1n << 63n)
const INT64_MAX = (1n << 63n) - 1n
export const MAX_UNIT_DECIMALS = 18

// amountUnits parses an exact int64 decimal string, or null.
export function amountUnits(amount: unknown): bigint | null {
  if (typeof amount !== "string" || !/^-?\d{1,19}$/.test(amount)) return null
  const value = BigInt(amount)
  return value < INT64_MIN || value > INT64_MAX ? null : value
}

export function isAmount(value: unknown): value is Amount {
  return amountUnits(value) !== null
}

export function isUnitDecimals(value: unknown): value is number {
  return (
    typeof value === "number" &&
    Number.isInteger(value) &&
    value >= 0 &&
    value <= MAX_UNIT_DECIMALS
  )
}

// addAmounts sums exact amounts; null when an input or the sum leaves int64.
export function addAmounts(...amounts: Amount[]): Amount | null {
  let total = 0n
  for (const amount of amounts) {
    const units = amountUnits(amount)
    if (units === null) return null
    total += units
  }
  return total < INT64_MIN || total > INT64_MAX ? null : total.toString()
}

// Intl.NumberFormat formats a decimal string exactly only since ECMA-402 2023
// (Chrome/Edge 106, Firefox 116, Safari 15.4); older engines coerce it through
// Number. Probed once with 2^53 + 1, which a Number cannot hold.
export const intlFormatsDecimalStringsExactly: boolean = (() => {
  try {
    return (
      new Intl.NumberFormat("en", { useGrouping: false }).format(
        "9007199254740993" as `${number}`
      ) === "9007199254740993"
    )
  } catch {
    return false
  }
})()

// Below 10^15 native units a Number-coerced decimal still rounds back to the
// exact digits at every registry scale; from 10^15 the coerced value can differ
// in the last digit, so a legacy engine refuses instead of misformatting.
const LEGACY_EXACT_UNITS = 10n ** 15n

function decimalFromUnits(value: bigint, decimals: number): string {
  const absolute = value < 0n ? -value : value
  const scale = 10n ** BigInt(decimals)
  const fraction = (absolute % scale)
    .toString()
    .padStart(decimals, "0")
    .replace(/0+$/, "")
  return `${value < 0n ? "-" : ""}${absolute / scale}${fraction ? `.${fraction}` : ""}`
}

// amountToDecimal renders native units as the exact major-unit decimal with
// trailing zeros trimmed ("99000000" at 6 -> "99", "1234567" -> "1.234567"),
// or null when the amount or scale is invalid.
export function amountToDecimal(
  amount: Amount,
  decimals: number
): string | null {
  const units = amountUnits(amount)
  if (units === null || !isUnitDecimals(decimals)) return null
  return decimalFromUnits(units, decimals)
}

// decimalToAmount parses a major-unit decimal exactly ("100.25" at 6 ->
// "100250000"). Excess precision, scientific notation and int64 overflow
// are refused; input is never rounded or converted through a JS number.
export function decimalToAmount(
  decimal: string,
  decimals: number
): Amount | null {
  if (!isUnitDecimals(decimals) || decimal.length > 40) return null
  const match = /^(-?)(\d+)(?:\.(\d+))?$/.exec(decimal)
  if (!match) return null
  const fraction = match[3] ?? ""
  if (fraction.length > decimals) return null
  const units =
    BigInt(match[2]) * 10n ** BigInt(decimals) +
    BigInt(fraction.padEnd(decimals, "0") || "0")
  const signed = match[1] ? -units : units
  return signed < INT64_MIN || signed > INT64_MAX ? null : signed.toString()
}

// formatAmount renders an exact amount in its currency for the buyer. It never
// passes money through a JS number; an amount it cannot show exactly is
// refused with a visible notice rather than a rounded figure.
export function formatAmount(
  amount: Amount | null | undefined,
  currency: string,
  decimals: number,
  locale?: string
): string {
  const code = currency.trim().toUpperCase()
  const units = amountUnits(amount)
  if (units === null || !isUnitDecimals(decimals))
    return `${code} amount exceeds the exact display range`
  if (
    !intlFormatsDecimalStringsExactly &&
    (units >= LEGACY_EXACT_UNITS || units <= -LEGACY_EXACT_UNITS)
  )
    return `${code} amount exceeds this browser's exact display range`
  const decimal = decimalFromUnits(units, decimals) as `${number}`
  try {
    return new Intl.NumberFormat(locale, {
      style: "currency",
      currency: code,
      maximumFractionDigits: decimals,
    }).format(decimal)
  } catch {
    return `${decimal} ${code}`
  }
}
