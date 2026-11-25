import { z } from "zod"

import { postalMode, type BillingFields } from "./billing-fields"

export {
  DEFAULT_BILLING_FIELDS,
  postalMode,
  type BillingFieldMode,
  type BillingFields,
} from "./billing-fields"

const ISO_COUNTRY_CODES =
  "AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW".split(
    " "
  )

const POSTAL_PLACEHOLDERS: Record<string, string> = {
  AR: "C1000",
  BR: "01000-000",
  CA: "A1A 1A1",
  CL: "8320000",
  CN: "100000",
  CO: "110111",
  DE: "10115",
  ES: "28001",
  JP: "100-0001",
  KR: "04524",
  MX: "01000",
  PE: "15001",
  US: "12345",
}

const displayNames = new Intl.DisplayNames(["en"], { type: "region" })

export const COUNTRY_OPTIONS = ISO_COUNTRY_CODES.map((code) => ({
  code,
  name: displayNames.of(code) ?? code,
})).sort((left, right) => left.name.localeCompare(right.name))

export const nameOnCardSchema = z
  .string()
  .trim()
  .min(1, "Full name is required")
  .max(200, "Full name is too long")

export const countryCodeSchema = z
  .string()
  .trim()
  .regex(/^[A-Za-z]{2}$/, "Select a country")
  .transform((value) => value.toUpperCase())

export function postalField(
  country: string,
  required: boolean
): {
  label: string
  placeholder: string
  inputMode: "numeric" | "text"
  pattern?: string
} {
  const code = country.trim().toUpperCase()
  const baseLabel =
    code === "US"
      ? "ZIP code"
      : code === "GB" || code === "AU" || code === "NZ"
        ? "Postcode"
        : "Postal code"
  return {
    label: required ? baseLabel : `${baseLabel} (optional)`,
    placeholder: POSTAL_PLACEHOLDERS[code] ?? baseLabel,
    inputMode: code === "US" ? "numeric" : "text",
    pattern: code === "US" ? "[0-9]{5}(?:-[0-9]{4})?" : undefined,
  }
}

/**
 * A card form's billing fields, as `fields` asks for them: a hidden field, or
 * an optional one left blank, is left out.
 */
export function billingSchema(fields: BillingFields) {
  return z
    .object({
      name_on_card: nameOnCardSchema,
      country: z.string().trim(),
      zip: z.string().trim().max(32, "Postal code is too long"),
    })
    .superRefine((value, context) => {
      const country = fields.country === "hidden" ? "" : value.country
      if (
        (fields.country === "required" || country) &&
        !/^[A-Za-z]{2}$/.test(country)
      )
        context.addIssue({
          code: "custom",
          message: "Select a country",
          path: ["country"],
        })
      const postal = postalMode(fields, country)
      if (postal === "required" && !value.zip)
        context.addIssue({
          code: "custom",
          message: `${postalField(country, true).label} is required`,
          path: ["zip"],
        })
      if (
        postal !== "hidden" &&
        country.toUpperCase() === "US" &&
        value.zip &&
        !/^\d{5}(?:-\d{4})?$/.test(value.zip)
      )
        context.addIssue({
          code: "custom",
          message: "Enter a valid ZIP code",
          path: ["zip"],
        })
    })
    .transform(({ name_on_card, country, zip }) => {
      const code = fields.country === "hidden" ? "" : country.toUpperCase()
      const postal = postalMode(fields, code) === "hidden" ? "" : zip
      return {
        name_on_card,
        ...(code ? { country: code } : {}),
        ...(postal ? { zip: postal } : {}),
      }
    })
}

/** What a card form's billing fields hold while the customer fills them. */
export interface NMIBilling {
  name_on_card: string
  country: string
  zip: string
}

/** A card form's billing fields as a saved card's billing details. */
export function billingDetailsOf(billing: {
  name_on_card?: string
  email?: string
  zip?: string
  country?: string
}) {
  const address = {
    ...(billing.zip ? { postal_code: billing.zip } : {}),
    ...(billing.country ? { country: billing.country } : {}),
  }
  return {
    ...(billing.name_on_card ? { name: billing.name_on_card } : {}),
    ...(billing.email ? { email: billing.email } : {}),
    ...(Object.keys(address).length > 0 ? { address } : {}),
  }
}

export const emptyNMIBilling: NMIBilling = {
  name_on_card: "",
  country: "",
  zip: "",
}

/** The buyer's likely billing country from the browser locale, or "". */
export function browserCountry(): string {
  try {
    if (typeof navigator === "undefined" || !navigator.language) return ""
    const region = new Intl.Locale(navigator.language).maximize().region
    return region && /^[A-Z]{2}$/.test(region) ? region : ""
  } catch {
    return ""
  }
}

/** The host's country when it is a known code, else the browser's region. */
export function initialCountry(hostCountry?: string): string {
  const code = hostCountry?.trim().toUpperCase() ?? ""
  if (COUNTRY_OPTIONS.some((option) => option.code === code)) return code
  const region = browserCountry()
  return COUNTRY_OPTIONS.some((option) => option.code === region) ? region : ""
}

// The EEA and the UK, where keeping a card for later one-off purchases needs
// the customer's unticked opt-in; mirrors OpenRails' mandates.optInCountries.
const REUSE_OPT_IN_COUNTRIES = new Set([
  "AT", "BE", "BG", "CY", "CZ", "DE", "DK", "EE", "ES", "FI", "FR", "GR", "HR",
  "HU", "IE", "IT", "LT", "LU", "LV", "MT", "NL", "PL", "PT", "RO", "SE", "SI",
  "SK", "IS", "LI", "NO", "GB",
])

export function reuseNeedsOptIn(country: string): boolean {
  return REUSE_OPT_IN_COUNTRIES.has(country.trim().toUpperCase())
}
