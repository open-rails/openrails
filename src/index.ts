// @openrails/billing-ui — styled OpenRails checkout and account billing.
// `Checkout`/`CheckoutModal` are the purchase flow; the account panels manage
// what was bought (they need `BillingProvider` from `./react`). The entry
// installs its isolated stylesheet once in browser environments.
import "./styles.css"

export { BillingUiProvider, type BillingUiProviderProps } from "./provider"
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
  checkoutRails,
  pspConfigSchema,
  savedMethodsFor,
  type CardSetupDriver,
  type CheckoutRailOffer,
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

export { Checkout, type CheckoutLayout, type CheckoutProps } from "./checkout"
export { CheckoutModal, type CheckoutModalProps } from "./modal"
export { CardBrandPlate } from "./components/card-brands"
export { resolveCardBrand, type CardBrand } from "./lib/card-brands"
export {
  createHttpSource,
  CheckoutSourceError,
  type CheckoutSource,
} from "./source"
export { createFixtureSource, fixtureSession } from "./fixtures"
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
  PaymentRailOption,
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
  payRequestSchema,
  payResultSchema,
  paymentRailOptionSchema,
  savedPaymentMethodSchema,
  unitDecimalsSchema,
} from "./types"
export {
  TokenizedCardForm,
  type TokenizedCardFormProps,
  type TokenizedCardData,
} from "./tokenized-card-form"
