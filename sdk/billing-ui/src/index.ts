// @openrails/billing-ui — styled OpenRails buying and account billing.
// `BuyButton`/`Offers` are the purchase; the account panels manage what was
// bought. `BillingProvider` (also in `./react`, without styles) gives them the
// client, appearance and words; the client and money formatting are here too,
// so a React app imports from one place. The entry installs its isolated
// stylesheet once in browser environments.
import "./styles.css"

export {
  createBillingClient,
  type BillingClient,
  type BillingClientOptions,
  type ProductListOptions,
} from "./client/client"
export {
  BillingProvider,
  type BillingProviderProps,
} from "./react/provider"
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
export { authenticatePayment } from "./authenticate"
export {
  canAuthenticatePayment,
  canSavePaymentMethod,
  cardSetupDriver,
  checkoutPsps,
  isCardRail,
  pspConfigSchema,
  savedMethodsFor,
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

export type { CheckoutLayout } from "./checkout"
export { BuyButton, type BuyButtonProps } from "./buy-button"
export { Offers, type OffersProps } from "./offers"
export { CheckoutPage, type CheckoutPageProps } from "./checkout-page"
export { CheckoutFrame, type CheckoutFrameProps } from "./checkout-frame"
export type { CheckoutFrameTheme } from "./frame"
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
  type CheckoutAppearance,
  type CheckoutAppearance as BillingAppearance,
  type CheckoutTheme,
  type CheckoutVariables,
} from "./appearance"
export type {
  CheckoutLineItem,
  CheckoutPhase,
  CheckoutPlan,
  CheckoutSession,
  CheckoutSessionStatus,
  NextAction,
  PaymentFailure,
  PaymentOption,
  PayRequest,
  PayResult,
  SavedPaymentMethod,
} from "./types"
export {
  amountSchema,
  checkoutLineItemSchema,
  checkoutPlanSchema,
  checkoutSessionSchema,
  checkoutSessionStatusSchema,
  nextActionSchema,
  paymentFailureSchema,
  payRequestSchema,
  payResultSchema,
  paymentOptionSchema,
  savedPaymentMethodSchema,
  unitDecimalsSchema,
} from "./types"
export {
  TokenizedCardForm,
  type TokenizedCardFormProps,
  type TokenizedCardData,
} from "./tokenized-card-form"
