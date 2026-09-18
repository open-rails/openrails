// Metering forms: what the console sends for a meter or a usage rate. Every
// amount is scaled to server units here, so a wrong scale is a wrong bill.
import { describe, expect, it } from "vitest"

import type { DefaultUsageRateCard, UsageMeter } from "@/lib/api/types"
import {
  buildMeterRequest,
  buildRateCardRequest,
  customerUsageRateRows,
  meterCollectionState,
  meterDefinitionLocked,
  negotiatedRateFormValues,
  negotiatedRateRequest,
  rateCardFormValues,
  RateCardFormError,
  summarizeRateCard,
  type RateCardFormValues,
} from "./metering-model"

const rate = (overrides: Partial<RateCardFormValues>): RateCardFormValues =>
  Object.assign(rateCardFormValues(), { productId: "prod_1" }, overrides)

const defaults = {
  id: "rate-1",
  product_id: "prod_1",
  product_key: "pro",
  filter: { region: ["us"] },
  price: {
    model: "per_unit" as const,
    currency: "USD",
    per_unit: { unit_amount: "1000000", divide_by: 1 },
  },
  created_at: "2026-08-17T00:00:00Z",
  updated_at: "2026-08-17T00:00:00Z",
} as DefaultUsageRateCard

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
        matrixCells: [
          { key: "fast", unitAmount: "0.01", maximumAmount: "20", included: "100" },
        ],
        filters: [{ key: "region", value: "eu, us, eu" }],
      })
    )
    expect(request.filter).toEqual({ region: ["eu", "us"] })
    expect(request.price.per_unit?.matrix).toEqual({
      dimension: "model",
      cells: {
        fast: { unit_amount: "10000", maximum_amount: "20000000", included: 100 },
      },
    })
  })

  it.each([
    [
      { allowanceMode: "included" as const, allowanceIncluded: "500" },
      { included: 500 },
    ],
    [
      {
        allowanceMode: "accrual" as const,
        allowanceAccrueFrom: " Active Seats ",
        allowanceCap: "30d",
      },
      { accrue_from: "active-seats", cap: "30d" },
    ],
  ])("builds the %o allowance", (values, allowance) => {
    expect(
      buildRateCardRequest(rate({ unitAmount: "1", ...values })).allowance
    ).toEqual(allowance)
  })

  it("addresses a row validation failure to the exact invalid control", () => {
    const invalid = rate({
      matrixEnabled: true,
      matrixDimension: "model",
      matrixCells: [{ key: "", unitAmount: "0.01", maximumAmount: "", included: "" }],
    })
    expect(() => buildRateCardRequest(invalid)).toThrow(RateCardFormError)
    try {
      buildRateCardRequest(invalid)
    } catch (error) {
      expect((error as RateCardFormError).fieldId).toBe("matrix-cell-0-value")
    }
  })
})

describe("negotiated rates", () => {
  const override = {
    meter_key: "tokens",
    price: {
      model: "package" as const,
      currency: "USD",
      package: { amount: "5000000", package_size: 1000 },
    },
    allowance: { included: 50 },
    created_at: "2026-08-17T00:00:00Z",
    updated_at: "2026-08-17T00:00:00Z",
  }

  it("prefills from the existing override, not the inherited default", () => {
    const values = negotiatedRateFormValues(defaults, override)
    expect(values.productId).toBe("prod_1")
    expect(values.model).toBe("package")
    expect(values.packageAmount).toBe("5")
    expect(values.allowanceIncluded).toBe("50")
    expect(values.filters).toEqual([{ key: "region", value: "us" }])
  })

  it("distinguishes an inherited allowance from an override without one", () => {
    const inherited = { ...defaults, allowance: { included: 100 } }
    expect(negotiatedRateFormValues(inherited).allowanceMode).toBe("included")
    expect(negotiatedRateFormValues(inherited).allowanceIncluded).toBe("100")
    const cleared = negotiatedRateFormValues(inherited, {
      ...override,
      price: defaults.price,
      allowance: undefined,
    })
    expect(cleared.allowanceMode).toBe("none")
    expect(cleared.allowanceIncluded).toBe("")
  })

  it("sends only the negotiated price and allowance", () => {
    const request = negotiatedRateRequest({
      product_id: "inherited-product",
      filter: { region: ["us"] },
      price: defaults.price,
      allowance: { included: 50 },
    })
    expect(request).toEqual({ price: defaults.price, allowance: { included: 50 } })
    expect(request).not.toHaveProperty("product_id")
    expect(request).not.toHaveProperty("filter")
  })
})

describe("metering state", () => {
  it.each([
    [{ pending: true, count: 0 }, "loading"],
    [{ pending: false, error: { status: 403 }, count: 0 }, "permission"],
    [{ pending: false, error: { status: 500 }, count: 0 }, "error"],
    [{ pending: false, count: 0 }, "empty"],
    [{ pending: false, count: 1 }, "ready"],
  ] as const)("maps %o to %s", (input, expected) => {
    expect(meterCollectionState(input)).toBe(expected)
  })

  it("names why a definition is locked", () => {
    const meter = { writes_allowed: false, has_activity: false } as UsageMeter
    expect(meterDefinitionLocked(meter)).toContain("manifest")
    expect(
      meterDefinitionLocked({ ...meter, writes_allowed: true, has_activity: true })
    ).toContain("first event")
  })

  it("joins negotiated rates only to billable meters with a default", () => {
    const ready = {
      key: "tokens",
      billing_supported: true,
      default_rate_card: { price: {} },
    } as UsageMeter
    const unsupported = { key: "unused", billing_supported: false } as UsageMeter
    const negotiated = {
      meter_key: "tokens",
      price: { model: "per_unit", currency: "USD" },
      created_at: "2026-08-17T00:00:00Z",
      updated_at: "2026-08-17T00:00:00Z",
    } as const
    expect(customerUsageRateRows([unsupported, ready], [negotiated])).toEqual([
      { meter: ready, override: negotiated },
    ])
  })

  it.each([
    [{ model: "per_unit", currency: "USD", per_unit: { unit_amount: "1000000", divide_by: 1 } }, "per 1 units"],
    [{ model: "tiered", currency: "USD", tiered: { mode: "volume", tiers: [{ up_to: null }] } }, "volume"],
    [{ model: "package", currency: "USD", package: { amount: "2000000", package_size: 100 } }, "per 100 units"],
  ])("summarizes %o", (price, summary) => {
    expect(
      summarizeRateCard({ price } as NonNullable<UsageMeter["default_rate_card"]>)
    ).toContain(summary)
  })
})
