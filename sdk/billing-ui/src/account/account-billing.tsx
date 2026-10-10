import { cn } from "cn"

import type { CheckoutAppearance } from "#orck/appearance"
import type { SendSolanaTransaction } from "#orck/client/client"
import { BillingUiRoot } from "#orck/scope"
import { PaymentHistory } from "./payment-history"
import { PaymentMethodsPanel } from "./payment-methods-panel"
import {
  SubscriptionsPanel,
  type SubscriptionsPanelProps,
} from "./subscriptions-panel"

export interface AccountBillingProps {
  /**
   * Offers to make a card the default for this currency: it pays the
   * currency's invoices and every subscription in it without its own card.
   */
  defaultCurrency?: string
  /** Billing country to preselect when adding a card. */
  defaultCountry?: string
  sendSolanaTransaction?: SendSolanaTransaction
  plansHref?: string
  renderSubscriptionFooter?: SubscriptionsPanelProps["renderSubscriptionFooter"]
  historyPageSize?: number
  appearance?: CheckoutAppearance
  className?: string
}

/** Subscriptions, payment methods and history in one column. */
export function AccountBilling({
  defaultCurrency,
  defaultCountry,
  sendSolanaTransaction,
  plansHref,
  renderSubscriptionFooter,
  historyPageSize,
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
    </BillingUiRoot>
  )
}
