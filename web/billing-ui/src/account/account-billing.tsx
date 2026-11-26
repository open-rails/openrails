import { cn } from "cn"

import type { BillingAppearance } from "#orck/appearance"
import type { SendSolanaTransaction } from "#orck/client/client"
import { BillingUiRoot } from "#orck/scope"
import { PaymentHistory } from "./payment-history"
import { PaymentMethodsPanel } from "./payment-methods-panel"
import { UsagePanel } from "./usage-panel"
import {
  SubscriptionsPanel,
  type SubscriptionsPanelProps,
} from "./subscriptions-panel"

export interface AccountBillingProps {
  /**
   * Offers to make a card the default for this currency: it pays the
   * currency's usage invoices and every subscription in it without its own card.
   */
  defaultCurrency?: string
  /** Billing country to preselect when adding a card. */
  defaultCountry?: string
  sendSolanaTransaction?: SendSolanaTransaction
  plansHref?: string
  renderSubscriptionFooter?: SubscriptionsPanelProps["renderSubscriptionFooter"]
  historyPageSize?: number
  /** Shows the customer's usage over time in this currency (or unit). */
  usageCurrency?: string
  appearance?: BillingAppearance
  className?: string
}

/** Subscriptions, payment methods, history and, with `usageCurrency`, usage in one column. */
export function AccountBilling({
  defaultCurrency,
  defaultCountry,
  sendSolanaTransaction,
  plansHref,
  renderSubscriptionFooter,
  historyPageSize,
  usageCurrency,
  appearance,
  className,
}: AccountBillingProps) {
  return (
    <BillingUiRoot
      appearance={appearance}
      className={cn("grid gap-6", className)}
      data-testid="account-billing"
    >
      <SubscriptionsPanel
        sendSolanaTransaction={sendSolanaTransaction}
        plansHref={plansHref}
        renderSubscriptionFooter={renderSubscriptionFooter}
        appearance={appearance}
      />
      <PaymentMethodsPanel
        defaultCurrency={defaultCurrency}
        defaultCountry={defaultCountry}
        appearance={appearance}
      />
      <PaymentHistory pageSize={historyPageSize} appearance={appearance} />
      {usageCurrency ? (
        <UsagePanel currency={usageCurrency} appearance={appearance} />
      ) : null}
    </BillingUiRoot>
  )
}
