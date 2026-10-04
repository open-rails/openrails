import { useCallback, useMemo, useState } from "react"

import {
  isWalletAction,
  signWalletAction,
  type SendSolanaTransaction,
} from "../client/client"
import { localError, toBillingError, type BillingError } from "../client/errors"
import type {
  NewCard,
  Payment,
  PaymentMethod,
  Product,
  Subscription,
  TierChange,
} from "../client/types"
import { useBillingContext } from "./context"
import { useRemote } from "./remote"

// Actions never throw: they resolve to null on success or the error.
export type ActionResult = Promise<BillingError | null>

/** `signing` and `confirming` are a wallet step's stages. */
export type SubscriptionAction =
  | "cancel"
  | "resume"
  | "payment_method"
  | "change_tier"
  | "signing"
  | "confirming"

export interface SubscriptionsOptions {
  /** Server filter; default `all`. */
  status?: string
  limit?: number
}

export interface SubscriptionsState {
  subscriptions: Subscription[] | null
  /** The next page's cursor; null on the last page. */
  nextCursor: string | null
  loading: boolean
  error: BillingError | null
  refetch: () => void
  /** In-flight action per subscription id. */
  pending: Readonly<Record<string, SubscriptionAction>>
  /**
   * Cancels at period end. A Solana subscription is canceled by the wallet:
   * pass `sendTransaction`; without it the cancel resolves to a
   * `wallet_required` error.
   */
  cancel: (
    subscriptionId: string,
    reason: string,
    sendTransaction?: SendSolanaTransaction
  ) => ActionResult
  resume: (subscriptionId: string) => ActionResult
  setPaymentMethod: (
    subscriptionId: string,
    paymentMethodId: string
  ) => ActionResult
  /**
   * Resolves to the change, whose `status` may still be `processing` or
   * `requires_action`, or to the error. A Solana change is signed with
   * `sendTransaction` and resolves once the chain confirms it.
   */
  changeTier: (
    subscriptionId: string,
    input: { priceId: string; idempotencyKey: string },
    sendTransaction?: SendSolanaTransaction
  ) => Promise<TierChange | BillingError>
}

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

const walletRequired = () =>
  localError("wallet_required", "A wallet must sign this action.")

export function useSubscriptions(
  options: SubscriptionsOptions = {}
): SubscriptionsState {
  const { client, version, notify } = useBillingContext()
  const status = options.status ?? "all"
  const limit = options.limit ?? 100
  const remote = useRemote(`${status}|${limit}|${version}`, (signal) =>
    client.listSubscriptions({ status, limit, signal })
  )
  const [pending, mark] = usePending<SubscriptionAction>()
  const { replace } = remote

  const patchRow = useCallback(
    (next: Subscription) =>
      replace((page) => ({
        ...page,
        data: page.data.map((s) => (s.id === next.id ? next : s)),
      })),
    [replace]
  )

  const act = useCallback(
    async (
      id: string,
      action: SubscriptionAction,
      run: () => Promise<Subscription>,
      done: (subscription: Subscription) => void
    ): ActionResult => {
      mark(id, action)
      try {
        const next = await run()
        patchRow(next)
        done(next)
        return null
      } catch (err) {
        return toBillingError(err)
      } finally {
        mark(id, null)
      }
    },
    [mark, patchRow]
  )

  // A declined wallet prompt resolves to a `wallet_rejected` error.
  const cancel = useCallback(
    (id: string, reason: string, sendTransaction?: SendSolanaTransaction) =>
      act(
        id,
        "cancel",
        async () => {
          const sub = await client.cancelSubscription(id, { reason })
          if (!sub.next_action) return sub
          if (!isWalletAction(sub.next_action) || !sendTransaction)
            throw walletRequired()
          mark(id, "signing")
          const signature = await signWalletAction(
            sub.next_action,
            sendTransaction
          )
          mark(id, "confirming")
          return client.cancelSubscription(id, { reason, signature })
        },
        () => notify({ type: "subscription.canceled", subscriptionId: id })
      ),
    [act, client, mark, notify]
  )

  const resume = useCallback(
    (id: string) =>
      act(
        id,
        "resume",
        () => client.resumeSubscription(id),
        () => notify({ type: "subscription.resumed", subscriptionId: id })
      ),
    [act, client, notify]
  )

  const setPaymentMethod = useCallback(
    (id: string, paymentMethodId: string) =>
      act(
        id,
        "payment_method",
        () => client.setSubscriptionPaymentMethod(id, paymentMethodId),
        () =>
          notify({
            type: "subscription.payment_method_changed",
            subscriptionId: id,
            paymentMethodId,
          })
      ),
    [act, client, notify]
  )

  // An upgrade may open a successor subscription: notify refetches the list.
  const changeTier = useCallback(
    async (
      id: string,
      input: { priceId: string; idempotencyKey: string },
      sendTransaction?: SendSolanaTransaction
    ) => {
      mark(id, "change_tier")
      try {
        let change = await client.changeTier(id, input)
        if (
          change.status === "requires_action" &&
          isWalletAction(change.next_action) &&
          sendTransaction
        ) {
          mark(id, "signing")
          const signature = await signWalletAction(
            change.next_action!,
            sendTransaction
          )
          mark(id, "confirming")
          change = await client.changeTier(id, { ...input, signature })
        }
        notify({
          type: "subscription.tier_changed",
          subscriptionId: id,
          change,
        })
        return change
      } catch (err) {
        return toBillingError(err)
      } finally {
        mark(id, null)
      }
    },
    [client, mark, notify]
  )

  return {
    subscriptions: remote.data?.data ?? null,
    nextCursor: remote.data?.next_cursor ?? null,
    loading: remote.loading,
    error: remote.error,
    refetch: remote.refetch,
    pending,
    cancel,
    resume,
    setPaymentMethod,
    changeTier,
  }
}

export interface ProductsState {
  products: Product[] | null
  /** The cursor of the next page; null on the last. */
  nextCursor: string | null
  loading: boolean
  error: BillingError | null
  refetch: () => void
}

/** The catalog: active products with their active prices. */
export function useProducts(options: { limit?: number } = {}): ProductsState {
  const { client } = useBillingContext()
  const limit = options.limit ?? 100
  const remote = useRemote(`${limit}`, (signal) =>
    client.listProducts({ limit, signal })
  )
  return {
    products: remote.data?.data ?? null,
    nextCursor: remote.data?.next_cursor ?? null,
    loading: remote.loading,
    error: remote.error,
    refetch: remote.refetch,
  }
}

export type PaymentMethodAction = "remove" | "collection"

export interface PaymentMethodsState {
  methods: PaymentMethod[] | null
  loading: boolean
  error: BillingError | null
  refetch: () => void
  pending: Readonly<Record<string, PaymentMethodAction>>
  adding: boolean
  add: (card: NewCard) => ActionResult
  remove: (paymentMethodId: string) => ActionResult
  /** Makes the card the one that collects one currency's invoices. */
  setCollection: (paymentMethodId: string, currency: string) => ActionResult
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

  const setCollection = useCallback(
    async (id: string, currency: string): ActionResult => {
      mark(id, "collection")
      try {
        await client.setCollectionPaymentMethod({
          currency,
          paymentMethodId: id,
        })
        const code = currency.toUpperCase()
        replace((page) => ({
          ...page,
          data: page.data.map((m) => {
            const others = (m.collection_currencies ?? []).filter(
              (c) => c !== code
            )
            return {
              ...m,
              collection_currencies: m.id === id ? [...others, code] : others,
            }
          }),
        }))
        notify({
          type: "payment_method.collection_changed",
          paymentMethodId: id,
        })
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
    setCollection,
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
  loading: boolean
  error: BillingError | null
  next: () => void
  previous: () => void
  refetch: () => void
}

export function usePayments(options: PaymentsOptions = {}): PaymentsState {
  const { client, version } = useBillingContext()
  const pageSize = options.pageSize ?? 20
  // cursors[i] reads page i of this filter; page 0 is the newest payments.
  const filter = `${pageSize}|${options.rail ?? ""}`
  const [state, setState] = useState({
    filter,
    cursors: [null] as (string | null)[],
  })
  const cursors = useMemo(
    () => (state.filter === filter ? state.cursors : [null]),
    [state, filter]
  )
  const page = cursors.length - 1
  const cursor = cursors[page]
  const remote = useRemote(`${filter}|${cursor ?? ""}|${version}`, (signal) =>
    client.listPayments({
      limit: pageSize,
      cursor,
      rail: options.rail,
      signal,
    })
  )
  const data = remote.data
  const nextCursor = data?.next_cursor ?? null
  return {
    payments: data?.data ?? null,
    page,
    hasMore: nextCursor !== null,
    loading: remote.loading,
    error: remote.error,
    next: useCallback(() => {
      if (nextCursor !== null)
        setState({ filter, cursors: [...cursors, nextCursor] })
    }, [filter, cursors, nextCursor]),
    previous: useCallback(() => {
      if (cursors.length > 1)
        setState({ filter, cursors: cursors.slice(0, -1) })
    }, [filter, cursors]),
    refetch: remote.refetch,
  }
}
