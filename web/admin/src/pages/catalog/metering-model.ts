import type {
  DefaultUsageRateCard,
  UsageAllowance,
  UsageMeter,
  UsageRatePrice,
} from "@/lib/api/types"
import type {
  DefaultUsageRateCardRequest,
  UsageMeterRequest,
} from "@/lib/api/endpoints"
import { formatMicros, microsFromInput } from "@/lib/format"

export interface KeyValueRow {
  key: string
  value: string
}

export interface MatrixCellRow {
  key: string
  unitAmount: string
  maximumAmount: string
  included: string
}

export interface TierRow {
  upTo: string
  unitAmount: string
  flatAmount: string
}

export interface MeterFormValues {
  key: string
  eventType: string
  aggregation: "sum" | "count"
  valueProperty: string
  unit: string
  groupBy: KeyValueRow[]
}

export type AllowanceMode = "none" | "included" | "accrual"

export interface RateCardFormValues {
  productId: string
  model: "per_unit" | "tiered" | "package"
  currency: string
  unitAmount: string
  divideBy: string
  round: "half_up" | "up" | "down"
  maximumAmount: string
  matrixEnabled: boolean
  matrixDimension: string
  matrixCells: MatrixCellRow[]
  tierMode: "volume" | "graduated"
  tiers: TierRow[]
  packageAmount: string
  packageSize: string
  freeUnits: string
  filters: KeyValueRow[]
  allowanceMode: AllowanceMode
  allowanceIncluded: string
  allowanceAccrueFrom: string
  allowanceCap: string
}

export class RateCardFormError extends Error {
  readonly fieldId: string

  constructor(fieldId: string, message: string) {
    super(message)
    this.name = "RateCardFormError"
    this.fieldId = fieldId
  }
}

export type MeterCollectionState =
  "loading" | "permission" | "error" | "empty" | "ready"

export function meterCollectionState(params: {
  pending: boolean
  error?: { status?: number } | null
  count: number
}): MeterCollectionState {
  if (params.pending) return "loading"
  if (params.error?.status === 403) return "permission"
  if (params.error) return "error"
  if (params.count === 0) return "empty"
  return "ready"
}

export function normalizeMeterKey(value: string): string {
  return value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, "-")
    .replace(/^[-_]+|[-_]+$/g, "")
}

export function meterFormValues(meter?: UsageMeter): MeterFormValues {
  return {
    key: meter?.key ?? "",
    eventType: meter?.event_type ?? "",
    aggregation: meter?.aggregation === "count" ? "count" : "sum",
    valueProperty: meter?.value_property ?? "",
    unit: meter?.unit ?? "",
    groupBy: Object.entries(meter?.group_by ?? {}).map(([key, value]) => ({
      key,
      value,
    })),
  }
}

export function buildMeterRequest(values: MeterFormValues): {
  key: string
  meter: UsageMeterRequest
} {
  const key = normalizeMeterKey(values.key)
  if (!key) throw new Error("Enter a meter key.")
  const eventType = values.eventType.trim()
  const valueProperty = values.valueProperty.trim()
  if (values.aggregation === "sum" && !valueProperty) {
    throw new Error("Sum meters require a value property.")
  }
  const groupBy = rowsToMap(values.groupBy, "group")
  return {
    key,
    meter: {
      event_type: eventType,
      aggregation: values.aggregation,
      value_property: values.aggregation === "sum" ? valueProperty : "",
      unit: values.unit.trim() || undefined,
      group_by: groupBy,
    },
  }
}

export function rateCardFormValues(
  card?: DefaultUsageRateCard
): RateCardFormValues {
  const price = card?.price
  const matrix = price?.per_unit?.matrix
  const allowance = card?.allowance
  const allowanceMode: AllowanceMode = allowance?.accrue_from
    ? "accrual"
    : allowance?.included !== undefined
      ? "included"
      : "none"
  return {
    productId: card?.product_id ?? "",
    model: price?.model ?? "per_unit",
    currency: price?.currency ?? "USD",
    unitAmount: microsToInput(price?.per_unit?.unit_amount),
    divideBy: integerToInput(price?.per_unit?.divide_by, "1"),
    round: price?.per_unit?.round ?? "half_up",
    maximumAmount: microsToInput(price?.per_unit?.maximum_amount),
    matrixEnabled: Boolean(matrix),
    matrixDimension: matrix?.dimension ?? "",
    matrixCells: Object.entries(matrix?.cells ?? {}).map(([key, cell]) => ({
      key,
      unitAmount: microsToInput(cell.unit_amount),
      maximumAmount: microsToInput(cell.maximum_amount),
      included: integerToInput(cell.included),
    })),
    tierMode: price?.tiered?.mode ?? "graduated",
    tiers: (
      price?.tiered?.tiers ?? [{ up_to: null, unit_amount: 0, flat_amount: 0 }]
    ).map((tier) => ({
      upTo: tier.up_to === null ? "" : String(tier.up_to),
      unitAmount: microsToInput(tier.unit_amount),
      flatAmount: microsToInput(tier.flat_amount),
    })),
    packageAmount: microsToInput(price?.package?.amount),
    packageSize: integerToInput(price?.package?.package_size, "1"),
    freeUnits: integerToInput(price?.package?.free_units),
    filters: Object.entries(card?.filter ?? {}).map(([key, values]) => ({
      key,
      value: values.join(", "),
    })),
    allowanceMode,
    allowanceIncluded: integerToInput(allowance?.included),
    allowanceAccrueFrom: allowance?.accrue_from ?? "",
    allowanceCap: allowance?.cap ?? "",
  }
}

export function buildRateCardRequest(
  values: RateCardFormValues
): DefaultUsageRateCardRequest {
  if (!values.productId) rateCardError("rate-product", "Select a product.")
  const currency = values.currency.trim().toUpperCase()
  if (!/^[A-Z]{3}$/.test(currency)) {
    rateCardError("rate-currency", "Enter a three-letter ISO currency.")
  }

  const price = buildPrice(values, currency)
  const allowance = buildAllowance(values)
  return {
    product_id: values.productId,
    filter: filterRowsToMap(values.filters),
    price,
    ...(allowance ? { allowance } : {}),
  }
}

function buildPrice(
  values: RateCardFormValues,
  currency: string
): UsageRatePrice {
  if (values.model === "per_unit") {
    const divideBy = positiveInteger(
      values.divideBy,
      "Divisor",
      "rate-divide-by"
    )
    const maximumAmount = optionalMoney(
      values.maximumAmount,
      "Maximum amount",
      "rate-maximum"
    )
    if (values.matrixEnabled) {
      const dimension = values.matrixDimension.trim()
      if (!dimension) {
        rateCardError("matrix-dimension", "Select a matrix dimension.")
      }
      if (values.matrixCells.length === 0) {
        rateCardError("matrix-add-cell", "Add at least one matrix cell.")
      }
      const cells: NonNullable<
        NonNullable<UsageRatePrice["per_unit"]>["matrix"]
      >["cells"] = {}
      for (const [index, row] of values.matrixCells.entries()) {
        const key = row.key.trim()
        const fieldPrefix = `matrix-cell-${index}`
        if (!key) {
          rateCardError(
            `${fieldPrefix}-value`,
            "Each matrix cell needs a value."
          )
        }
        if (cells[key]) {
          rateCardError(
            `${fieldPrefix}-value`,
            `Duplicate matrix value “${key}”.`
          )
        }
        cells[key] = {
          unit_amount: requiredMoney(
            row.unitAmount,
            `Price for ${key}`,
            `${fieldPrefix}-unit-price`,
            true
          ),
          ...(optionalMoney(
            row.maximumAmount,
            `Maximum for ${key}`,
            `${fieldPrefix}-maximum`
          ) > 0
            ? {
                maximum_amount: optionalMoney(
                  row.maximumAmount,
                  `Maximum for ${key}`,
                  `${fieldPrefix}-maximum`
                ),
              }
            : {}),
          ...(optionalWhole(
            row.included,
            `Included units for ${key}`,
            `${fieldPrefix}-included`
          ) > 0
            ? {
                included: optionalWhole(
                  row.included,
                  `Included units for ${key}`,
                  `${fieldPrefix}-included`
                ),
              }
            : {}),
        }
      }
      return {
        model: "per_unit",
        currency,
        per_unit: {
          divide_by: divideBy,
          round: values.round,
          ...(maximumAmount > 0 ? { maximum_amount: maximumAmount } : {}),
          matrix: { dimension, cells },
        },
      }
    }
    return {
      model: "per_unit",
      currency,
      per_unit: {
        unit_amount: requiredMoney(
          values.unitAmount,
          "Unit price",
          "rate-unit-amount",
          true
        ),
        divide_by: divideBy,
        round: values.round,
        ...(maximumAmount > 0 ? { maximum_amount: maximumAmount } : {}),
      },
    }
  }

  if (values.model === "tiered") {
    if (values.tiers.length === 0) {
      rateCardError("tier-add", "Add at least one tier.")
    }
    const tiers = values.tiers.map((row, index) => {
      const last = index === values.tiers.length - 1
      const upTo = row.upTo.trim()
      if (last && upTo) {
        rateCardError(
          `tier-${index}-limit`,
          "The final tier must be unbounded."
        )
      }
      if (!last && !upTo) {
        rateCardError(
          `tier-${index}-limit`,
          "Only the final tier can be unbounded."
        )
      }
      return {
        up_to: last
          ? null
          : positiveInteger(
              upTo,
              `Tier ${index + 1} limit`,
              `tier-${index}-limit`
            ),
        unit_amount: requiredMoney(
          row.unitAmount,
          `Tier ${index + 1} unit price`,
          `tier-${index}-unit-price`,
          true
        ),
        flat_amount: optionalMoney(
          row.flatAmount,
          `Tier ${index + 1} flat amount`,
          `tier-${index}-flat-amount`
        ),
      }
    })
    for (let index = 1; index < tiers.length - 1; index += 1) {
      if ((tiers[index].up_to ?? 0) <= (tiers[index - 1].up_to ?? 0)) {
        rateCardError(`tier-${index}-limit`, "Tier limits must increase.")
      }
    }
    return {
      model: "tiered",
      currency,
      tiered: { mode: values.tierMode, tiers },
    }
  }

  return {
    model: "package",
    currency,
    package: {
      amount: requiredMoney(
        values.packageAmount,
        "Package price",
        "package-amount"
      ),
      package_size: positiveInteger(
        values.packageSize,
        "Package size",
        "package-size"
      ),
      free_units: optionalWhole(values.freeUnits, "Free units", "package-free"),
    },
  }
}

function buildAllowance(
  values: RateCardFormValues
): UsageAllowance | undefined {
  if (values.allowanceMode === "none") return undefined
  if (values.allowanceMode === "included") {
    return {
      included: optionalWhole(
        values.allowanceIncluded,
        "Included allowance",
        "allowance-included"
      ),
    }
  }
  const accrueFrom = normalizeMeterKey(values.allowanceAccrueFrom)
  if (!accrueFrom) {
    rateCardError("allowance-source", "Select the allowance source meter.")
  }
  const cap = values.allowanceCap.trim().toLowerCase()
  if (!/^\d+[hd]$/.test(cap)) {
    rateCardError(
      "allowance-cap",
      "Allowance cap must be a whole number of hours or days."
    )
  }
  return { accrue_from: accrueFrom, cap }
}

function rowsToMap(rows: KeyValueRow[], name: string): Record<string, string> {
  const result: Record<string, string> = {}
  for (const row of rows) {
    const key = row.key.trim()
    const value = row.value.trim()
    if (!key || !value) throw new Error(`Each ${name} row needs both fields.`)
    if (result[key]) throw new Error(`Duplicate ${name} key “${key}”.`)
    result[key] = value
  }
  return result
}

function filterRowsToMap(rows: KeyValueRow[]): Record<string, string[]> {
  const result: Record<string, string[]> = {}
  for (const [index, row] of rows.entries()) {
    const key = row.key.trim()
    const values = [
      ...new Set(row.value.split(",").map((value) => value.trim())),
    ].filter(Boolean)
    if (!key) {
      rateCardError(
        `filter-${index}-dimension`,
        "Select a dimension for this filter."
      )
    }
    if (values.length === 0) {
      rateCardError(
        `filter-${index}-values`,
        "Enter at least one value for this filter."
      )
    }
    if (result[key]) {
      rateCardError(
        `filter-${index}-dimension`,
        `Duplicate filter dimension “${key}”.`
      )
    }
    result[key] = values
  }
  return result
}

function requiredMoney(
  value: string,
  label: string,
  fieldId: string,
  allowZero = false
): number {
  const amount = microsFromInput(value)
  if (amount === null || amount < 0 || (!allowZero && amount === 0)) {
    rateCardError(
      fieldId,
      `${label} must be ${allowZero ? "zero or more" : "greater than zero"}.`
    )
  }
  return amount
}

function optionalMoney(value: string, label: string, fieldId: string): number {
  if (!value.trim()) return 0
  return requiredMoney(value, label, fieldId, true)
}

function positiveInteger(
  value: string,
  label: string,
  fieldId: string
): number {
  const amount = Number(value)
  if (!Number.isSafeInteger(amount) || amount <= 0) {
    rateCardError(fieldId, `${label} must be a positive whole number.`)
  }
  return amount
}

function optionalWhole(value: string, label: string, fieldId: string): number {
  if (!value.trim()) return 0
  const amount = Number(value)
  if (!Number.isSafeInteger(amount) || amount < 0) {
    rateCardError(fieldId, `${label} must be zero or a positive whole number.`)
  }
  return amount
}

function rateCardError(fieldId: string, message: string): never {
  throw new RateCardFormError(fieldId, message)
}

function microsToInput(value?: number): string {
  if (!value) return ""
  return String(value / 1_000_000)
}

function integerToInput(value?: number, fallback = ""): string {
  return value ? String(value) : fallback
}

export function summarizeRateCard(card?: DefaultUsageRateCard): string {
  if (!card) return "Not priced"
  const { price } = card
  if (price.model === "per_unit" && price.per_unit) {
    if (price.per_unit.matrix) {
      return `${Object.keys(price.per_unit.matrix.cells).length} matrix rates · ${price.currency}`
    }
    return `${formatMicros(price.per_unit.unit_amount ?? 0, price.currency)} per ${price.per_unit.divide_by || 1} units`
  }
  if (price.model === "tiered" && price.tiered) {
    return `${price.tiered.tiers.length} ${price.tiered.mode} tiers · ${price.currency}`
  }
  if (price.model === "package" && price.package) {
    return `${formatMicros(price.package.amount, price.currency)} per ${price.package.package_size.toLocaleString()} units`
  }
  return `Unsupported price · ${price.currency}`
}

export function meterDefinitionLocked(meter: UsageMeter): string | null {
  if (!meter.writes_allowed)
    return "This meter is managed by the catalog manifest."
  if (meter.has_activity) return "Meter definitions lock after the first event."
  return null
}
