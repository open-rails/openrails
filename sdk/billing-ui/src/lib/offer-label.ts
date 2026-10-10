import { formatMoney } from "../account/format"
import type { CurrencyScales, Price } from "../client/types"
import type { Translator } from "../i18n/messages"
import { periodOf } from "./period"

/** "$4.99", "Rent for 3 days, $1.99" or "$10.00 every 30 days". */
export function offerLabel(
  price: Pick<
    Price,
    | "unit_amount"
    | "currency"
    | "billing_interval_hours"
    | "access_duration_hours"
  >,
  scales: CurrencyScales,
  m: Translator,
  locale?: string
): string {
  const amount =
    formatMoney(price.unit_amount, price.currency, scales, locale) ??
    `${price.unit_amount} ${price.currency}`
  const every = periodOf(price.billing_interval_hours)
  if (every)
    return m.t("offers.recurring", {
      amount,
      every: m.plural(`interval.every.${every.unit}`, every.count),
    })
  const access = periodOf(price.access_duration_hours)
  if (access)
    return m.t("offers.rent", {
      amount,
      period: m.plural(`interval.duration.${access.unit}`, access.count),
    })
  return m.t("offers.once", { amount })
}
