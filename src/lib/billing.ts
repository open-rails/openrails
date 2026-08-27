import { z } from "zod"

const ISO_COUNTRY_CODES =
  "AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW".split(
    " "
  )

// ISO-3166 alpha-2 projection of the UPU's "List of countries which do not
// require postal codes" (Universal DataBase, September 2025). Keep a postal
// code when a customer supplies one, but do not block tokenization when the
// destination does not require it.
// https://www.upu.int/UPU/media/upu/documents/PostCode/General-Addressing-Issues.pdf
const POSTAL_OPTIONAL_COUNTRIES = new Set([
  "AE",
  "AG",
  "AO",
  "AQ",
  "AW",
  "BF",
  "BI",
  "BJ",
  "BO",
  "BQ",
  "BS",
  "BV",
  "BW",
  "BZ",
  "CF",
  "CG",
  "CI",
  "CK",
  "CM",
  "CW",
  "DM",
  "ER",
  "FJ",
  "GA",
  "GD",
  "GM",
  "GQ",
  "JM",
  "KM",
  "KP",
  "LY",
  "ML",
  "MR",
  "QA",
  "RW",
  "SB",
  "SC",
  "SL",
  "SO",
  "SR",
  "SS",
  "ST",
  "SX",
  "SY",
  "TD",
  "TG",
  "TK",
  "TO",
  "TV",
  "UM",
  "VU",
  "YE",
  "ZW",
])

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
  .min(1, "Name on card is required")
  .max(200, "Name on card is too long")

export const countryCodeSchema = z
  .string()
  .trim()
  .regex(/^[A-Za-z]{2}$/, "Select a country")
  .transform((value) => value.toUpperCase())

export function isPostalRequired(country: string): boolean {
  const code = country.trim().toUpperCase()
  return code === "" || !POSTAL_OPTIONAL_COUNTRIES.has(code)
}

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

export const nmiBillingSchema = z
  .object({
    name_on_card: nameOnCardSchema,
    country: countryCodeSchema,
    zip: z.string().trim().max(32, "Postal code is too long"),
  })
  .superRefine((value, context) => {
    if (isPostalRequired(value.country) && !value.zip) {
      context.addIssue({
        code: "custom",
        message: `${postalField(value.country, true).label} is required`,
        path: ["zip"],
      })
    }
    if (
      value.country === "US" &&
      value.zip &&
      !/^\d{5}(?:-\d{4})?$/.test(value.zip)
    ) {
      context.addIssue({
        code: "custom",
        message: "Enter a valid ZIP code",
        path: ["zip"],
      })
    }
  })

export type NMIBilling = z.infer<typeof nmiBillingSchema>

export const emptyNMIBilling: NMIBilling = {
  name_on_card: "",
  country: "",
  zip: "",
}
