import { describe, expect, it } from "vitest"

import {
  COUNTRY_OPTIONS,
  isPostalRequired,
  nmiBillingSchema,
  postalField,
} from "./billing"

describe("NMI billing identity", () => {
  it("preserves one canonical international name and normalizes country", () => {
    expect(
      nmiBillingSchema.parse({
        name_on_card: "  李 小龍  ",
        country: "jp",
        zip: "100-0001",
      })
    ).toEqual({
      name_on_card: "李 小龍",
      country: "JP",
      zip: "100-0001",
    })
  })

  it("matches the UPU postal-optional country list exactly", () => {
    const expectedOptionalCountries =
      "AE AG AO AQ AW BF BI BJ BO BQ BS BV BW BZ CF CG CI CK CM CW DM ER FJ GA GD GM GQ JM KM KP LY ML MR QA RW SB SC SL SO SR SS ST SX SY TD TG TK TO TV UM VU YE ZW".split(
        " "
      )

    expect(
      COUNTRY_OPTIONS.map(({ code }) => code)
        .filter((code) => !isPostalRequired(code))
        .sort()
    ).toEqual(expectedOptionalCountries)
  })

  it("allows missing postal data for UPU-optional countries", () => {
    expect(isPostalRequired("AG")).toBe(false)
    expect(
      nmiBillingSchema.safeParse({
        name_on_card: "Cher",
        country: "AG",
        zip: "",
      }).success
    ).toBe(true)
  })

  it("requires postal data for countries removed from the optional list", () => {
    expect(isPostalRequired("IE")).toBe(true)
    expect(
      nmiBillingSchema.safeParse({
        name_on_card: "Cher",
        country: "IE",
        zip: "",
      }).success
    ).toBe(false)
  })

  it("uses country-aware labels and conservative US validation", () => {
    expect(postalField("US", true)).toMatchObject({
      label: "ZIP code",
      placeholder: "12345",
      inputMode: "numeric",
    })
    expect(postalField("GB", true).label).toBe("Postcode")
    expect(
      nmiBillingSchema.safeParse({
        name_on_card: "Ada Lovelace",
        country: "US",
        zip: "1234",
      }).success
    ).toBe(false)
    expect(
      nmiBillingSchema.safeParse({
        name_on_card: "Ada Lovelace",
        country: "US",
        zip: "12345-6789",
      }).success
    ).toBe(true)
  })
})
