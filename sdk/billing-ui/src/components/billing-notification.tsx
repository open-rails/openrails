import { useMessages } from "../i18n/context"
import {
  createTranslator,
  resolveMessages,
  type Translator,
  type BillingUiMessageBundle,
} from "../i18n/messages"
import { BillingUiRoot } from "../scope"
import type { BillingAppearance } from "../appearance"

import {
  dataOf,
  isPaymentFailure,
  isBillingNotification,
  billingNotificationAction,
  type BillingNotificationItem,
  type BillingNotificationActions,
} from "../lib/billing-notification"

/** Shared billing wording. Hosts supply only their authorized destination URLs. */
export function BillingNotification({
  item,
  actions = {},
  language,
  messages,
  appearance,
}: {
  item: BillingNotificationItem
  actions?: BillingNotificationActions
  language?: string
  messages?: BillingUiMessageBundle
  appearance?: BillingAppearance
}) {
  const inherited = useMessages()
  const translator =
    language || messages
      ? createTranslator(
          messages ? resolveMessages(messages) : inherited.messages,
          undefined,
          language ?? inherited.locale
        )
      : inherited
  const { t } = translator
  if (!isBillingNotification(item)) return null
  const data = dataOf(item)
  const title =
    item.kind === "payment_method_update_required"
      ? t("billingNotifications.paymentMethodRequiredTitle")
      : item.kind === "payment_authentication_required"
        ? t("billingNotifications.authenticationRequiredTitle")
        : item.kind === "order_collection_stopped"
          ? t("billingNotifications.collectionStoppedTitle")
          : isPaymentFailure(item.kind)
            ? t("billingNotifications.paymentFailed")
            : item.kind === "premium_ended"
              ? t("billingNotifications.membershipEnded")
              : t("billingNotifications.membershipDowngraded")
  let detail: string
  if (isPaymentFailure(item.kind)) {
    const attempt = date(data.next_attempt_at, translator)
    detail =
      data.retry_status === "scheduled" && attempt
        ? t("billingNotifications.retryScheduled", { date: attempt })
        : data.retry_status === "payment_method_required"
          ? t("billingNotifications.paymentMethodRequired")
          : data.retry_status === "stopped"
            ? t("billingNotifications.retryStopped")
            : t("billingNotifications.retryUnknown")
  } else if (item.kind === "premium_ended") {
    const ended = date(data.ended_at, translator)
    detail = ended
      ? t("billingNotifications.endedAt", { date: ended })
      : t("billingNotifications.ended")
  } else {
    detail =
      typeof data.from_product_name === "string" &&
      typeof data.to_product_name === "string" &&
      data.from_product_name &&
      data.to_product_name
        ? t("billingNotifications.downgradedTo", {
            from: data.from_product_name,
            to: data.to_product_name,
          })
        : t("billingNotifications.downgraded")
  }
  const action = billingNotificationAction(item)
  const href = actions[action] ?? actions.manage_billing
  const label = actions[action] ? action : "manage_billing"
  return (
    <BillingUiRoot
      appearance={appearance}
      className="grid gap-2"
      data-orck="billing-notification"
    >
      <p className="font-medium">{title}</p>
      <p className="text-muted-foreground">{detail}</p>
      {isPaymentFailure(item.kind) && (
        <p className="text-muted-foreground">
          {t(`billingNotifications.instructions.${action}`)}
        </p>
      )}
      {href && (
        <a href={href} className="font-medium underline underline-offset-4">
          {t(`billingNotifications.actions.${label}`)}
        </a>
      )}
    </BillingUiRoot>
  )
}

function date(value: unknown, translator: Translator): string | undefined {
  if (typeof value !== "string" || !Number.isFinite(Date.parse(value)))
    return undefined
  return new Intl.DateTimeFormat(translator.locale, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value))
}
