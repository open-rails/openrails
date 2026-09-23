import { useCallback, useState } from "react"

import type { SendSolanaTransaction, SolanaCancelStage } from "../client/client"
import { toBillingError, type BillingError } from "../client/errors"
import type {
  NewCard,
  Payment,
  PaymentMethod,
  Subscription,
} from "../client/types"
import { useBillingContext } from "./context"
import { sleep, useRemote } from "./remote"

// Actions never throw: they resolve to null on success or the error.
export type ActionResult = Promise<BillingError | null>

export type SubscriptionAction =
  "cancel" | "resume" | "payment_method" | SolanaCancelStage

export interface SubscriptionsOptions {
  /** Server filter; default `all`. */
  status?: string
  limit?: number
}

export interface SubscriptionsState {
  subscriptions: Subscription[] | null
  total: number | null
  loading: boolean
  error: BillingError | null
  refetch: () => void
  /** In-flight action per subscription id. */
  pending: Readonly<Record<string, SubscriptionAction>>
  cancel: (subscriptionId: string, feedback: string) => ActionResult
  /** Solana rail: the wallet signs the server-prepared cancel transaction. */
  cancelOnChain: (
    subscriptionId: string,
    sendTransaction: SendSolanaTransaction
  ) => ActionResult
  resume: (subscriptionId: string) => ActionResult
  setPaymentMethod: (
    subscriptionId: string,
    paymentMethodId: string
  ) => ActionResult
}

const cancelApplied = (s: Subscription) =>
  !!s.cancel_scheduled || s.status === "cancelled" || !!s.resumable
const resumeApplied = (s: Subscription) =>
  !s.cancel_scheduled && s.status !== "cancelled"

function usePending<A extends string>() {
  const [pending, setPending] = useState<Readonly<Record<string, A>>>({})
  const mark = useCallback((id: string, action: A | null) => {
    setPending((p) => {
      const next = { ...p }
      if (action) next[id] = action
      else delete next[id]
      return next
    })
  }, [])
  return [pending, mark] as const
}

export function useSubscriptions(
  options: SubscriptionsOptions = {}
): SubscriptionsState {
  const { client, version, notify, settle } = useBillingContext()
  const status = options.status ?? "all"
  const limit = options.limit ?? 100
  const remote = useRemote(`${status}|${limit}|${version}`, (signal) =>
    client.listSubscriptions({ status, limit, signal })
  )
  const [pending, mark] = usePending<SubscriptionAction>()
  const { replace } = remote

  // Re-reads a queued change until it shows, then patches the row.
  const settleRow = useCallback(
    async (id: string, applied: (s: Subscription) => boolean) => {
      for (let i = 0; i < settle.attempts; i++) {
        const next = await client.getSubscription(id).catch(() => null)
        if (next && applied(next)) {
          replace((page) => ({
            ...page,
            data: page.data.map((s) => (s.id === id ? next : s)),
          }))
          return true
        }
        await sleep(settle.intervalMs)
      }
      return false
    },
    [client, replace, settle.attempts, settle.intervalMs]
  )

  const act = useCallback(
    async (
      id: string,
      action: SubscriptionAction,
      run: () => Promise<boolean | void>,
      done?: (settled: boolean) => void
    ): ActionResult => {
      mark(id, action)
      try {
        const settled = await run()
        done?.(settled !== false)
        return null
      } catch (err) {
        return toBillingError(err)
      } finally {
        mark(id, null)
      }
    },
    [mark]
  )

  const cancel = useCallback(
    (id: string, feedback: string) =>
      act(
        id,
        "cancel",
        async () => {
          await client.cancelSubscription(id, { feedback })
          return settleRow(id, cancelApplied)
        },
        (settled) =>
          notify({
            type: "subscription.cancelled",
            subscriptionId: id,
            settled,
          })
      ),
    [act, client, notify, settleRow]
  )

  // A declined wallet prompt resolves to a `wallet_rejected` error.
  const cancelOnChain = useCallback(
    (id: string, sendTransaction: SendSolanaTransaction) =>
      act(
        id,
        "preparing",
        async () => {
          await client.cancelSubscriptionOnChain(id, sendTransaction, (stage) =>
            mark(id, stage)
          )
          return settleRow(id, cancelApplied)
        },
        (settled) =>
          notify({
            type: "subscription.cancelled",
            subscriptionId: id,
            settled,
          })
      ),
    [act, client, mark, notify, settleRow]
  )

  const resume = useCallback(
    (id: string) =>
      act(
        id,
        "resume",
        async () => {
          await client.resumeSubscription(id)
          return settleRow(id, resumeApplied)
        },
        (settled) =>
          notify({ type: "subscription.resumed", subscriptionId: id, settled })
      ),
    [act, client, notify, settleRow]
  )

  const setPaymentMethod = useCallback(
    (id: string, paymentMethodId: string) =>
      act(id, "payment_method", async () => {
        await client.setSubscriptionPaymentMethod(id, paymentMethodId)
        replace((page) => ({
          ...page,
          data: page.data.map((s) =>
            s.id === id ? { ...s, payment_method_id: paymentMethodId } : s
          ),
        }))
      }),
    [act, client, replace]
  )

  return {
    subscriptions: remote.data?.data ?? null,
    total: remote.data?.total ?? null,
    loading: remote.loading,
    error: remote.error,
    refetch: remote.refetch,
    pending,
    cancel,
    cancelOnChain,
    resume,
    setPaymentMethod,
  }
}

export type PaymentMethodAction = "remove" | "default"

export interface PaymentMethodsState {
  methods: PaymentMethod[] | null
  loading: boolean
  error: BillingError | null
  refetch: () => void
  pending: Readonly<Record<string, PaymentMethodAction>>
  adding: boolean
  add: (card: NewCard) => ActionResult
  remove: (paymentMethodId: string) => ActionResult
  /** Default collector for one currency's invoices. */
  setDefault: (paymentMethodId: string, currency: string) => ActionResult
}

export function usePaymentMethods(): PaymentMethodsState {
  const { client, version, notify } = useBillingContext()
  const remote = useRemote(`${version}`, (signal) =>
    client.listPaymentMethods({ signal })
  )
  const [pending, mark] = usePending<PaymentMethodAction>()
  const [adding, setAdding] = useState(false)
  const { replace } = remote

  const add = useCallback(
    async (card: NewCard): ActionResult => {
      setAdding(true)
      try {
        const method = await client.addPaymentMethod(card)
        replace((page) => ({ ...page, data: [...page.data, method] }))
        notify({ type: "payment_method.added", paymentMethodId: method.id })
        return null
      } catch (err) {
        return toBillingError(err)
      } finally {
        setAdding(false)
      }
    },
    [client, notify, replace]
  )

  const remove = useCallback(
    async (id: string): ActionResult => {
      mark(id, "remove")
      try {
        await client.removePaymentMethod(id)
        replace((page) => ({
          ...page,
          data: page.data.filter((m) => m.id !== id),
        }))
        notify({ type: "payment_method.removed", paymentMethodId: id })
        return null
      } catch (err) {
        return toBillingError(err)
      } finally {
        mark(id, null)
      }
    },
    [client, mark, notify, replace]
  )

  const setDefault = useCallback(
    async (id: string, currency: string): ActionResult => {
      mark(id, "default")
      try {
        await client.setDefaultPaymentMethod({ currency, paymentMethodId: id })
        const code = currency.toUpperCase()
        replace((page) => ({
          ...page,
          data: page.data.map((m) => {
            const others = (m.collection_default_currencies ?? []).filter(
              (c) => c !== code
            )
            return {
              ...m,
              collection_default_currencies:
                m.id === id ? [...others, code] : others,
            }
          }),
        }))
        notify({ type: "payment_method.default_changed", paymentMethodId: id })
        return null
      } catch (err) {
        return toBillingError(err)
      } finally {
        mark(id, null)
      }
    },
    [client, mark, notify, replace]
  )

  return {
    methods: remote.data?.data ?? null,
    loading: remote.loading,
    error: remote.error,
    refetch: remote.refetch,
    pending,
    adding,
    add,
    remove,
    setDefault,
  }
}

export interface PaymentsOptions {
  pageSize?: number
  /** Filter by rail. */
  rail?: string
}

export interface PaymentsState {
  payments: Payment[] | null
  /** Zero-based. */
  page: number
  hasMore: boolean
  total: number | null
  loading: boolean
  error: BillingError | null
  next: () => void
  previous: () => void
  refetch: () => void
}

export function usePayments(options: PaymentsOptions = {}): PaymentsState {
  const { client, version } = useBillingContext()
  const pageSize = options.pageSize ?? 20
  const [page, setPage] = useState(0)
  const remote = useRemote(
    `${pageSize}|${options.rail ?? ""}|${page}|${version}`,
    (signal) =>
      client.listPayments({
        limit: pageSize,
        offset: page * pageSize,
        rail: options.rail,
        signal,
      })
  )
  const data = remote.data
  const total = data?.total ?? null
  const hasMore =
    data?.has_more ??
    (total !== null
      ? (page + 1) * pageSize < total
      : data?.data.length === pageSize)
  return {
    payments: data?.data ?? null,
    page,
    hasMore: !!hasMore,
    total,
    loading: remote.loading,
    error: remote.error,
    next: useCallback(() => setPage((p) => p + 1), []),
    previous: useCallback(() => setPage((p) => Math.max(0, p - 1)), []),
    refetch: remote.refetch,
  }
}
