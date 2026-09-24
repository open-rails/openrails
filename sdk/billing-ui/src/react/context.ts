import { createContext, useContext } from "react"

import type { BillingClient } from "../client/client"

export type BillingChange =
  | {
      type: "subscription.cancelled" | "subscription.resumed"
      subscriptionId: string
      /** False when the server had not applied the queued change yet. */
      settled: boolean
    }
  | {
      type: "subscription.payment_method_changed"
      subscriptionId: string
      paymentMethodId: string
    }
  | {
      type:
        | "payment_method.added"
        | "payment_method.removed"
        | "payment_method.default_changed"
      paymentMethodId: string
    }

export interface BillingContextValue {
  client: BillingClient
  /** Bumped after every mutation; hooks refetch on change. */
  version: number
  notify(change: BillingChange): void
  /** Refetch every hook after a host-side change such as a plan switch. */
  refresh(): void
  settle: { intervalMs: number; attempts: number }
}

export const BillingContext = createContext<BillingContextValue | null>(null)

export function useBillingContext(): BillingContextValue {
  const ctx = useContext(BillingContext)
  if (!ctx) throw new Error("billing-ui: wrap the tree in <BillingProvider>")
  return ctx
}

export const useBillingClient = (): BillingClient => useBillingContext().client

export const useBillingRefresh = (): (() => void) => useBillingContext().refresh

/** The billing context when a BillingProvider is present, else null. */
export const useOptionalBillingContext = (): BillingContextValue | null =>
  useContext(BillingContext)
