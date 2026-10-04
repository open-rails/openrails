// Native card entry (driver "card"): the PSP declares card_entry: server, so
// the page posts the card to OpenRails, which vaults it at the gateway. No
// gateway script is loaded. The card lives in component state only until it
// is submitted.
import * as React from "react"

import type { CollectFieldErrors } from "#orck/lib/collect"

/** A card as OpenRails' `card` field takes it. */
export interface CardEntry {
  number: string
  exp_month: number
  exp_year: number
  cvc: string
}

/** What the buyer typed, as typed. */
export interface CardEntryInput {
  number: string
  expiry: string
  cvc: string
}

export const FIELD_MESSAGES: Required<CollectFieldErrors> = {
  number: "Enter a valid card number",
  expiry: "Enter a valid expiry date",
  cvv: "Enter a valid security code",
}

const digitsOf = (value: string) => value.replace(/\D/g, "")

function luhn(digits: string): boolean {
  let sum = 0
  for (let i = 0; i < digits.length; i++) {
    let d = digits.charCodeAt(digits.length - 1 - i) - 48
    if (i % 2 === 1) {
      d *= 2
      if (d > 9) d -= 9
    }
    sum += d
  }
  return sum % 10 === 0
}

/** The card network a number's leading digits name ("" when none). */
export function cardBrandOf(number: string): string {
  const d = digitsOf(number)
  const p = (n: number) => (d.length >= n ? Number(d.slice(0, n)) : -1)
  if (p(1) === 4) return "visa"
  if (p(2) === 34 || p(2) === 37) return "amex"
  if ((p(2) >= 51 && p(2) <= 55) || (p(4) >= 2221 && p(4) <= 2720))
    return "mastercard"
  if (p(4) === 6011 || p(2) === 65 || (p(3) >= 644 && p(3) <= 649))
    return "discover"
  if (p(2) === 35) return "jcb"
  if (p(2) === 36 || p(2) === 38 || (p(3) >= 300 && p(3) <= 305))
    return "diners"
  return ""
}

/** Groups a number as it is printed: 4-6-5 for Amex, fours otherwise. */
export function formatCardNumber(value: string): string {
  const d = digitsOf(value).slice(0, 19)
  if (cardBrandOf(d) === "amex")
    return [d.slice(0, 4), d.slice(4, 10), d.slice(10, 15)]
      .filter(Boolean)
      .join(" ")
  return d.replace(/(\d{4})(?=\d)/g, "$1 ")
}

/** MM / YY as it is typed. */
export function formatExpiry(value: string): string {
  const d = digitsOf(value).slice(0, 4)
  return d.length > 2 ? `${d.slice(0, 2)} / ${d.slice(2)}` : d
}

function parseExpiry(value: string, now: Date) {
  const d = digitsOf(value)
  if (d.length !== 4 && d.length !== 6) return null
  const month = Number(d.slice(0, 2))
  const year = d.length === 4 ? 2000 + Number(d.slice(2)) : Number(d.slice(2))
  if (month < 1 || month > 12) return null
  const lapsed =
    year < now.getFullYear() ||
    (year === now.getFullYear() && month < now.getMonth() + 1)
  return lapsed ? null : { month, year }
}

/** Every field's error; an empty object means the card is complete. */
export function cardEntryErrors(
  input: CardEntryInput,
  now = new Date()
): CollectFieldErrors {
  const errors: CollectFieldErrors = {}
  const number = digitsOf(input.number)
  if (number.length < 12 || number.length > 19 || !luhn(number))
    errors.number = FIELD_MESSAGES.number
  if (!parseExpiry(input.expiry, now)) errors.expiry = FIELD_MESSAGES.expiry
  const cvc = digitsOf(input.cvc)
  const want = cardBrandOf(number) === "amex" ? 4 : 3
  if (cvc !== input.cvc.trim() || (cvc.length !== want && cvc.length !== 4))
    errors.cvv = FIELD_MESSAGES.cvv
  return errors
}

/** The card OpenRails takes, or null while any field is invalid. */
export function parseCardEntry(
  input: CardEntryInput,
  now = new Date()
): CardEntry | null {
  if (Object.keys(cardEntryErrors(input, now)).length > 0) return null
  const expiry = parseExpiry(input.expiry, now)!
  return {
    number: digitsOf(input.number),
    exp_month: expiry.month,
    exp_year: expiry.year,
    cvc: digitsOf(input.cvc),
  }
}

/** A card's display facts, the ones a saved card is labelled with. */
export function cardEntryDisplay(card: CardEntry): {
  last_four: string
  card_type?: string
  expiry_date: string
} {
  const brand = cardBrandOf(card.number)
  return {
    last_four: card.number.slice(-4),
    ...(brand ? { card_type: brand } : {}),
    expiry_date: `${String(card.exp_month).padStart(2, "0")}/${String(card.exp_year % 100).padStart(2, "0")}`,
  }
}

const empty: CardEntryInput = { number: "", expiry: "", cvc: "" }

/**
 * The state of one native card form. Errors show once a field was left;
 * `take` hands the card over and clears the form, so the page keeps it no
 * longer than the request that sends it.
 */
export function useCardEntry() {
  const [input, setInput] = React.useState<CardEntryInput>(empty)
  const [touched, setTouched] = React.useState<
    Partial<Record<keyof CardEntryInput, boolean>>
  >({})
  const errors = cardEntryErrors(input)
  const shown: CollectFieldErrors = {}
  if (touched.number && errors.number) shown.number = errors.number
  if (touched.expiry && errors.expiry) shown.expiry = errors.expiry
  if (touched.cvc && errors.cvv) shown.cvv = errors.cvv
  const clear = React.useCallback(() => {
    setInput(empty)
    setTouched({})
  }, [])
  return {
    input,
    fieldErrors: shown,
    valid: Object.keys(errors).length === 0,
    change: (field: keyof CardEntryInput, value: string) =>
      setInput((current) => ({
        ...current,
        [field]:
          field === "number"
            ? formatCardNumber(value)
            : field === "expiry"
              ? formatExpiry(value)
              : digitsOf(value).slice(0, 4),
      })),
    leave: (field: keyof CardEntryInput) =>
      setTouched((current) => ({ ...current, [field]: true })),
    take: (): CardEntry | null => {
      const card = parseCardEntry(input)
      if (!card) {
        setTouched({ number: true, expiry: true, cvc: true })
        return null
      }
      clear()
      return card
    },
    clear,
  }
}

export type CardEntryState = ReturnType<typeof useCardEntry>
