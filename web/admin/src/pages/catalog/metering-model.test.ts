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
  type MeterFormValues,
  type RateCardFormValues,
} from "./metering-model"

const baseRate = (): RateCardFormValues => rateCardFormValues()

describe("meter form", () => {
  it("normalizes a sum meter and structured dimensions", () => {
    const values: MeterFormValues = {
      key: " API Tokens ",
      eventType: "token.used",
      aggregation: "sum",
      valueProperty: "tokens",
      unit: "tokens",
      groupBy: [{ key: "model", value: "metadata.model" }],
    }
    expect(buildMeterRequest(values)).toEqual({
      key: "api-tokens",
      meter: {
        event_type: "token.used",
        value_property: "tokens",
        aggregation: "sum",
        unit: "tokens",
        group_by: { model: "metadata.model" },
      },
    })
  })

  it("clears the value property for count meters", () => {
    const result = buildMeterRequest({
      key: "requests",
      eventType: "request.completed",
      aggregation: "count",
      valueProperty: "ignored",
      unit: "requests",
      groupBy: [],
    })
    expect(result.meter.value_property).toBe("")
  })

  it("rejects a sum meter without a value property", () => {
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

describe("rate-card form", () => {
  it("builds direct per-unit pricing", () => {
    const values = baseRate()
    Object.assign(values, {
      productId: "product-1",
      unitAmount: "0.0025",
      divideBy: "1000",
      maximumAmount: "15",
    })
    expect(buildRateCardRequest(values).price).toEqual({
      model: "per_unit",
      currency: "USD",
      per_unit: {
        unit_amount: 2500,
        divide_by: 1000,
        round: "half_up",
        maximum_amount: 15_000_000,
      },
    })
  })

  it("builds a per-unit matrix and filters", () => {
    const values = baseRate()
    Object.assign(values, {
      productId: "product-1",
      matrixEnabled: true,
      matrixDimension: "model",
      matrixCells: [
        {
          key: "fast",
          unitAmount: "0.01",
          maximumAmount: "20",
          included: "100",
        },
      ],
      filters: [{ key: "region", value: "eu, us, eu" }],
    })
    const request = buildRateCardRequest(values)
    expect(request.filter).toEqual({ region: ["eu", "us"] })
    expect(request.price.per_unit?.matrix).toEqual({
      dimension: "model",
      cells: {
        fast: {
          unit_amount: 10_000,
          maximum_amount: 20_000_000,
          included: 100,
        },
      },
    })
  })

  it("builds graduated tiers with one unbounded final tier", () => {
    const values = baseRate()
    Object.assign(values, {
      productId: "product-1",
      model: "tiered",
      tiers: [
        { upTo: "100", unitAmount: "0.2", flatAmount: "" },
        { upTo: "", unitAmount: "0.1", flatAmount: "5" },
      ],
    })
    expect(buildRateCardRequest(values).price.tiered?.tiers).toEqual([
      { up_to: 100, unit_amount: 200_000, flat_amount: 0 },
      { up_to: null, unit_amount: 100_000, flat_amount: 5_000_000 },
    ])
  })

  it("builds package pricing and a flat allowance", () => {
    const values = baseRate()
    Object.assign(values, {
      productId: "product-1",
      model: "package",
      packageAmount: "2.5",
      packageSize: "1000",
      freeUnits: "25",
      allowanceMode: "included",
      allowanceIncluded: "500",
    })
    const request = buildRateCardRequest(values)
    expect(request.price.package).toEqual({
      amount: 2_500_000,
      package_size: 1000,
      free_units: 25,
    })
    expect(request.allowance).toEqual({ included: 500 })
  })

  it("builds an accrued allowance", () => {
    const values = baseRate()
    Object.assign(values, {
      productId: "product-1",
      unitAmount: "1",
      allowanceMode: "accrual",
      allowanceAccrueFrom: " Active Seats ",
      allowanceCap: "30d",
    })
    expect(buildRateCardRequest(values).allowance).toEqual({
      accrue_from: "active-seats",
      cap: "30d",
    })
  })

  it("addresses row validation to the exact invalid control", () => {
    const values = baseRate()
    Object.assign(values, {
      productId: "product-1",
      matrixEnabled: true,
      matrixDimension: "model",
      matrixCells: [
        { key: "", unitAmount: "0.01", maximumAmount: "", included: "" },
      ],
    })

    try {
      buildRateCardRequest(values)
      throw new Error("expected validation to fail")
    } catch (error) {
      expect(error).toBeInstanceOf(RateCardFormError)
      expect((error as RateCardFormError).fieldId).toBe("matrix-cell-0-value")
    }
  })

  it("prefills negotiated editing from the override over inherited defaults", () => {
    const defaults = {
      id: "rate-1",
      product_id: "product-1",
      product_key: "pro",
      filter: { region: ["us"] },
      price: {
        model: "per_unit" as const,
        currency: "USD",
        per_unit: { unit_amount: 1_000_000, divide_by: 1 },
      },
      created_at: "2026-08-17T00:00:00Z",
      updated_at: "2026-08-17T00:00:00Z",
    } as DefaultUsageRateCard
    const values = negotiatedRateFormValues(defaults, {
      meter_key: "tokens",
      price: {
        model: "package",
        currency: "USD",
        package: { amount: 5_000_000, package_size: 1000 },
      },
      allowance: { included: 50 },
      created_at: "2026-08-17T00:00:00Z",
      updated_at: "2026-08-17T00:00:00Z",
    })

    expect(values.productId).toBe("product-1")
    expect(values.currency).toBe("USD")
    expect(values.model).toBe("package")
    expect(values.packageAmount).toBe("5")
    expect(values.allowanceIncluded).toBe("50")
    expect(values.filters).toEqual([{ key: "region", value: "us" }])
  })

  it("sends only the negotiated price and allowance", () => {
    const request = negotiatedRateRequest({
      product_id: "inherited-product",
      filter: { region: ["us"] },
      price: {
        model: "per_unit",
        currency: "USD",
        per_unit: { unit_amount: 500_000, divide_by: 1 },
      },
      allowance: { included: 50 },
    })

    expect(request).toEqual({
      price: {
        model: "per_unit",
        currency: "USD",
        per_unit: { unit_amount: 500_000, divide_by: 1 },
      },
      allowance: { included: 50 },
    })
    expect(request).not.toHaveProperty("product_id")
    expect(request).not.toHaveProperty("filter")
  })
})

describe("metering states", () => {
  it.each([
    [{ pending: true, count: 0 }, "loading"],
    [{ pending: false, error: { status: 403 }, count: 0 }, "permission"],
    [{ pending: false, error: { status: 500 }, count: 0 }, "error"],
    [{ pending: false, count: 0 }, "empty"],
    [{ pending: false, count: 1 }, "ready"],
  ] as const)("maps %o to %s", (input, expected) => {
    expect(meterCollectionState(input)).toBe(expected)
  })

  it("distinguishes manifest and activity locks", () => {
    const meter = {
      writes_allowed: false,
      has_activity: false,
    } as UsageMeter
    expect(meterDefinitionLocked(meter)).toContain("manifest")
    expect(
      meterDefinitionLocked({
        ...meter,
        writes_allowed: true,
        has_activity: true,
      })
    ).toContain("first event")
  })

  it("joins negotiated rates only to billable meters with defaults", () => {
    const ready = {
      key: "tokens",
      billing_supported: true,
      default_rate_card: { price: {} },
    } as UsageMeter
    const unsupported = {
      key: "unused",
      billing_supported: false,
    } as UsageMeter
    const override = {
      meter_key: "tokens",
      price: { model: "per_unit", currency: "USD" },
      created_at: "2026-08-17T00:00:00Z",
      updated_at: "2026-08-17T00:00:00Z",
    } as const

    expect(customerUsageRateRows([unsupported, ready], [override])).toEqual([
      { meter: ready, override },
    ])
  })

  it("summarizes each supported model", () => {
    const card = (
      price: UsageMeter["default_rate_card"] extends infer T ? T : never
    ) => price
    expect(
      summarizeRateCard(
        card({
          price: {
            model: "per_unit",
            currency: "USD",
            per_unit: { unit_amount: 1_000_000, divide_by: 1 },
          },
        } as NonNullable<UsageMeter["default_rate_card"]>)
      )
    ).toContain("per 1 units")
    expect(
      summarizeRateCard(
        card({
          price: {
            model: "tiered",
            currency: "USD",
            tiered: { mode: "volume", tiers: [{ up_to: null }] },
          },
        } as NonNullable<UsageMeter["default_rate_card"]>)
      )
    ).toContain("volume")
    expect(
      summarizeRateCard(
        card({
          price: {
            model: "package",
            currency: "USD",
            package: { amount: 2_000_000, package_size: 100 },
          },
        } as NonNullable<UsageMeter["default_rate_card"]>)
      )
    ).toContain("per 100 units")
  })
})
