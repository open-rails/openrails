// @openrails/billing-ui — the OpenRails checkout flow as a self-contained
// component. `Checkout` is the flow; render it inline anywhere. `CheckoutModal`
// is a ready-made dialog host around the same flow. The package entry installs
// its isolated stylesheet once in browser environments.
import "./styles.css"

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
