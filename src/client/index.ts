// Framework-free client for the OpenRails customer surface (`/billing/v1/me/*`).
export {
  CANCEL_FEEDBACK_MAX,
  CANCEL_FEEDBACK_MIN,
  createBillingClient,
  isWalletRejection,
  WalletRejectedError,
  type BillingClient,
  type BillingClientOptions,
  type ListOptions,
  type SendSolanaTransaction,
  type SolanaCancelStage,
} from "./client"
export {
  BillingError,
  isBillingError,
  readBillingError,
  toBillingError,
  type BillingErrorBody,
} from "./errors"
export type {
  BillingStatus,
  CardSummary,
  CurrencyScales,
  Invoice,
  NewCard,
  Page,
  Payment,
  PaymentMethod,
  PaymentOperation,
  PaymentRecovery,
  Subscription,
  SubscriptionPrice,
  SubscriptionProduct,
  SubscriptionStatus,
} from "./types"
export { amountToDecimal, formatAmount, type Amount } from "../lib/money"
