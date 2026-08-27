import { describe, expect, it } from "vitest"

import { isPostalRequired, nmiBillingSchema, postalField } from "./billing"

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

  it("requires postal data only where the country uses it", () => {
    expect(
      nmiBillingSchema.safeParse({
        name_on_card: "Cher",
        country: "US",
        zip: "",
      }).success
    ).toBe(false)
    expect(
      nmiBillingSchema.safeParse({
        name_on_card: "Cher",
        country: "IE",
        zip: "",
      }).success
    ).toBe(true)
    expect(isPostalRequired("IE")).toBe(false)
    expect(isPostalRequired("JP")).toBe(true)
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
