// Metering forms: what the console sends for a meter or a usage rate. Every
// amount is scaled to server units here, so a wrong scale is a wrong bill.
import { describe, expect, it } from "vitest"

import {
  buildMeterRequest,
  buildRateCardRequest,
  rateCardFormValues,
  RateCardFormError,
  type RateCardFormValues,
} from "./metering-model"

const rate = (overrides: Partial<RateCardFormValues>): RateCardFormValues =>
  Object.assign(rateCardFormValues(), { productId: "prod_1" }, overrides)

describe("meter definition", () => {
  it("slugs the key, structures dimensions and clears an unused property", () => {
    expect(
      buildMeterRequest({
        key: " API Tokens ",
        eventType: "token.used",
        aggregation: "sum",
        valueProperty: "tokens",
        unit: "tokens",
        groupBy: [{ key: "model", value: "metadata.model" }],
      })
    ).toEqual({
      key: "api-tokens",
      meter: {
        event_type: "token.used",
        value_property: "tokens",
        aggregation: "sum",
        unit: "tokens",
        group_by: { model: "metadata.model" },
      },
    })
    expect(
      buildMeterRequest({
        key: "requests",
        eventType: "request.completed",
        aggregation: "count",
        valueProperty: "ignored",
        unit: "requests",
        groupBy: [],
      }).meter.value_property
    ).toBe("")
    expect(() =>
      buildMeterRequest({
        key: "tokens",
        eventType: "",
        aggregation: "sum",
        valueProperty: "",
        unit: "",
        groupBy: [],
      })
    ).toThrow("Sum meters require a value property")
  })
})

describe("usage rate amounts", () => {
  it("round-trips a dynamic card without inventing a price block", () => {
    const values = rateCardFormValues({
      product_id: "prod_1", price: { model: "dynamic", currency: "USD" },
      filter: {}, allowance: null,
    })
    expect(values.model).toBe("dynamic")
    expect(buildRateCardRequest(values)).toEqual({
      product_id: "prod_1", filter: {}, price: { model: "dynamic", currency: "USD" },
    })
    expect(buildRateCardRequest(rate({
      model: "dynamic", allowanceMode: "included", allowanceIncluded: "500",
    })).allowance).toBeUndefined()
  })

  it.each([
    [
      "per-unit with a divisor and a cap",
      { unitAmount: "0.0025", divideBy: "1000", maximumAmount: "15" },
      {
        model: "per_unit",
        currency: "USD",
        per_unit: {
          unit_amount: "2500",
          divide_by: 1000,
          round: "half_up",
          maximum_amount: "15000000",
        },
      },
    ],
    [
      "graduated tiers ending unbounded",
      {
        model: "tiered" as const,
        tiers: [
          { upTo: "100", unitAmount: "0.2", flatAmount: "" },
          { upTo: "", unitAmount: "0.1", flatAmount: "5" },
        ],
      },
      {
        model: "tiered",
        currency: "USD",
        tiered: {
          mode: "graduated",
          tiers: [
            { up_to: 100, unit_amount: "200000", flat_amount: "0" },
            { up_to: null, unit_amount: "100000", flat_amount: "5000000" },
          ],
        },
      },
    ],
    [
      "package pricing with free units",
      {
        model: "package" as const,
        packageAmount: "2.5",
        packageSize: "1000",
        freeUnits: "25",
      },
      {
        model: "package",
        currency: "USD",
        package: { amount: "2500000", package_size: 1000, free_units: 25 },
      },
    ],
  ])("scales %s to server units", (_name, values, price) => {
    expect(buildRateCardRequest(rate(values)).price).toEqual(price)
  })

  it("scales matrix cells and normalizes filters", () => {
    const request = buildRateCardRequest(
      rate({
        matrixEnabled: true,
        matrixDimension: "model",
        matrixCells: [{ key: "fast", unitAmount: "0.01", maximumAmount: "20" }],
        filters: [{ key: "region", value: "eu, us, eu" }],
      })
    )
    expect(request.filter).toEqual({ region: ["eu", "us"] })
    expect(request.price.per_unit?.matrix).toEqual({
      dimension: "model",
      cells: { fast: { unit_amount: "10000", maximum_amount: "20000000" } },
    })
  })

  it("builds an included allowance", () => {
    expect(
      buildRateCardRequest(
        rate({
          unitAmount: "1",
          allowanceMode: "included",
          allowanceIncluded: "500",
        })
      ).allowance
    ).toEqual({ included: 500 })
  })

  it("addresses a row validation failure to the exact invalid control", () => {
    const invalid = rate({
      matrixEnabled: true,
      matrixDimension: "model",
      matrixCells: [{ key: "", unitAmount: "0.01", maximumAmount: "" }],
    })
    expect(() => buildRateCardRequest(invalid)).toThrow(RateCardFormError)
    try {
      buildRateCardRequest(invalid)
    } catch (error) {
      expect((error as RateCardFormError).fieldId).toBe("matrix-cell-0-value")
    }
  })
})
