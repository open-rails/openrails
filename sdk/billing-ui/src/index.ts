// @openrails/billing-ui — styled OpenRails buying and account billing.
// `BuyButton`/`ProductList` are the purchase; the account panels manage what was
// bought. `BillingProvider` (also in `./react`, without styles) gives them the
// client, appearance and words; the client and money formatting are here too,
// so a React app imports from one place. The entry installs its isolated
// stylesheet once in browser environments.
import "./styles.css"

export {
  createBillingClient,
  type BillingClient,
  type BillingClientOptions,
} from "./client/client"
export {
  redeemAssertion,
  tokenSource,
  type RedeemAssertionOptions,
  type TokenResponse,
  type TokenSource,
  type TokenSourceOptions,
} from "./client/token-source"
export { BillingProvider, type BillingProviderProps } from "./react/provider"
export { BillingUiRoot } from "./scope"
export type { Navigate } from "./scope-context"
export {
  AccountBilling,
  type AccountBillingProps,
} from "./account/account-billing"
export {
  SubscriptionsPanel,
  type SubscriptionsPanelProps,
} from "./account/subscriptions-panel"
export {
  RenewalPrompt,
  type RenewalPromptProps,
} from "./account/renewal-prompt"
export {
  CancelSubscriptionDialog,
  type CancelSubscriptionDialogProps,
} from "./account/cancel-dialog"
export {
  PaymentMethodsPanel,
  type PaymentMethodsPanelProps,
} from "./account/payment-methods-panel"
export {
  SavePaymentMethod,
  type SavePaymentMethodProps,
} from "./save-payment-method"
export { OrderPayment, type OrderPaymentProps } from "./order-payment"
export {
  PAYMENT_COMPLETE_MESSAGE,
  PAYMENT_RESIZE_MESSAGE,
  EmbeddedPayment,
  type EmbeddedPaymentCompletion,
  type EmbeddedPaymentProps,
} from "./embedded-payment"
export { Cart, type CartProps } from "./cart"
export {
  readPaymentFragment,
  withPaymentFallbacks,
  type PaymentFallback,
  type PaymentFallbacks,
} from "./fallbacks"
export { authenticateChange } from "./authenticate"
export {
  canAuthenticatePayment,
  canSavePaymentMethod,
  cardSetupDriver,
  routedPsps,
  pspConfigSchema,
  type CardSetupDriver,
  type PspConfig,
} from "./psp"
export {
  PaymentHistory,
  type PaymentHistoryProps,
} from "./account/payment-history"
export {
  BillingStatusBadge,
  type BillingStatusBadgeProps,
} from "./account/status-badge"
export { statusTone, type StatusTone } from "./account/format"
export {
  createTranslator,
  defaultMessages,
  defineMessages,
  interpolate,
  resolveMessages,
  useMessages,
  type BillingUiMessageBundle,
  type BillingUiMessages,
  type BillingUiTranslate,
  type MessageKey,
  type PluralKey,
  type PluralMessage,
  type MessageVars,
  type Translator,
} from "./i18n"

export { BuyButton, type BuyButtonProps } from "./buy-button"
export { ProductList, type ProductListProps } from "./product-list"
export { CardBrandPlate } from "./components/card-brands"
export { resolveCardBrand, type CardBrand } from "./lib/card-brands"
export { browserCountry, initialCountry } from "./lib/billing"
export {
  addAmounts,
  amountToDecimal,
  formatAmount,
  isAmount,
  type Amount,
} from "./lib/money"
export {
  type BillingAppearance,
  type BillingTheme,
  type BillingVariables,
} from "./appearance"
export type { SavedPaymentMethod } from "./types"
export {
  TokenizedCardForm,
  type TokenizedCardFormProps,
  type TokenizedCardData,
} from "./tokenized-card-form"

export { BillingNotification } from "./components/billing-notification"
export {
  isBillingNotification,
  billingNotificationAction,
  type BillingNotificationItem,
  type BillingNotificationActions,
  type BillingNotificationAction,
} from "./lib/billing-notification"
