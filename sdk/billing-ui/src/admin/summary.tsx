import { AdminMessage } from "#orck/admin/ui-context"
import { useCallback, useEffect, useMemo, useState } from "react"
import { appearanceStyle } from "../appearance"
import type { AdminAccess } from "../client/generated/wire"
import { ApiError, createAdminClient } from "./lib/api/client"
import { createAdminAPI } from "./lib/api"
import { formatNativeAmount } from "./lib/format"
import type { BillingAdminProps } from "./types"
import { useAdminTheme } from "./theme"

type Connection = Pick<
  BillingAdminProps,
  "baseUrl" | "auth" | "merchant" | "exchange"
>

function useAPI(options: Connection) {
  const id =
    typeof options.merchant === "string"
      ? options.merchant
      : options.merchant?.id
  const slug =
    typeof options.merchant === "string"
      ? options.merchant
      : options.merchant?.slug
  return useMemo(
    () =>
      createAdminAPI(
        createAdminClient({
          baseUrl: options.baseUrl,
          auth: options.auth,
          exchange: options.exchange,
          merchant: id ? { id, slug: slug ?? id } : undefined,
        })
      ),
    [options.baseUrl, options.auth, options.exchange, id, slug]
  )
}

function useRead<T>(load: (signal: AbortSignal) => Promise<T>) {
  const [attempt, setAttempt] = useState(0)
  const [result, setResult] = useState<{
    load: typeof load
    attempt: number
    data?: T
    error?: Error
  }>()
  useEffect(() => {
    const abort = new AbortController()
    load(abort.signal).then(
      (data) => {
        if (!abort.signal.aborted) setResult({ load, attempt, data })
      },
      (error) => {
        if (!abort.signal.aborted)
          setResult({
            load,
            attempt,
            error: error instanceof Error ? error : new Error(String(error)),
          })
      }
    )
    return () => abort.abort()
  }, [load, attempt])
  const current =
    result?.load === load && result.attempt === attempt ? result : undefined
  return {
    data: current?.data,
    error: current?.error,
    isPending: !current,
    refetch: () => setAttempt((n) => n + 1),
  }
}

/** Reads tab permissions without requiring a BillingAdmin or QueryClient provider. */
export function useBillingAdminAccess(options: Connection) {
  const api = useAPI(options)
  return useRead<AdminAccess>(
    useCallback((signal) => api.getAdminAccess(signal), [api])
  )
}

export interface BillingCustomerSummaryProps extends Connection {
  customer: string
  href?: string
  appearance?: BillingAdminProps["appearance"]
  className?: string
}

// An account that never reached billing has no customer yet: that is a state, not a failure.
const missingCustomer = (error: Error) =>
  error instanceof ApiError && error.code === "customer_not_found"

/** A bounded preview for the host's customer or user page. */
export function BillingCustomerSummary(props: BillingCustomerSummaryProps) {
  const theme = useAdminTheme(props.appearance?.theme)
  const api = useAPI(props)
  const { data, error, isPending, refetch } = useRead(
    useCallback(
      async (signal: AbortSignal) => {
        const [customer, subscriptions, entitlements, access, payments] =
          await Promise.all([
            api.getCustomer(props.customer, signal),
            api.listSubscriptions(
              { customer_id: props.customer, status: "active" },
              5,
              undefined,
              signal
            ),
            api.listCustomerEntitlements(props.customer, { limit: 5 }, signal),
            api.listCustomerProductAccess(props.customer, { limit: 5 }, signal),
            api.listPayments(
              { customer_id: props.customer },
              { limit: 5 },
              signal
            ),
          ])
        return { customer, subscriptions, entitlements, access, payments }
      },
      [api, props.customer]
    )
  )
  return (
    <section
      className={["orba", props.className].filter(Boolean).join(" ")}
      data-orba-theme={theme}
      style={appearanceStyle(props.appearance, "orba")}
    >
      <div className="rounded-lg border p-4">
        <h3 className="font-semibold">
          <AdminMessage id={"Billing"} />
        </h3>
        {isPending && (
          <p role="status">
            <AdminMessage id={"Loading billing…"} />
          </p>
        )}
        {error && missingCustomer(error) && (
          <p>
            <AdminMessage id={"No billing activity yet."} />
          </p>
        )}
        {error && !missingCustomer(error) && (
          <p role="alert">
            {error.message}{" "}
            <button type="button" onClick={refetch}>
              <AdminMessage id={"Try again"} />
            </button>
          </p>
        )}
        {data && (
          <>
            <dl>
              {data.customer.balances.map((balance) => (
                <div key={balance.currency}>
                  <dt>
                    <AdminMessage id={"Available balance"} />
                  </dt>
                  <dd>
                    {formatNativeAmount(
                      balance.available_amount,
                      balance.currency
                    )}
                  </dd>
                </div>
              ))}
            </dl>
            <h4>
              <AdminMessage id={"Active subscriptions"} />
            </h4>
            {data.subscriptions.data.length ? (
              <ul>
                {data.subscriptions.data.map((subscription) => (
                  <li key={subscription.id}>{subscription.id}</li>
                ))}
              </ul>
            ) : (
              <p>
                <AdminMessage id={"No active subscriptions."} />
              </p>
            )}
            <h4>
              <AdminMessage id={"Entitlements"} />
            </h4>
            <ul>
              {data.entitlements.data.map((entitlement) => (
                <li key={entitlement.entitlement}>{entitlement.entitlement}</li>
              ))}
            </ul>
            <h4>
              <AdminMessage id={"Product access"} />
            </h4>
            <ul>
              {data.access.data.map((access) => (
                <li key={access.id}>
                  {access.product_name} · {access.status}
                </li>
              ))}
            </ul>
            <h4>
              <AdminMessage id={"Recent payments"} />
            </h4>
            <ul>
              {data.payments.data.map((payment) => (
                <li key={payment.id}>
                  {formatNativeAmount(payment.amount, payment.currency)} ·{" "}
                  {payment.status}
                </li>
              ))}
            </ul>
          </>
        )}
        {props.href && (
          <a className="underline" href={props.href}>
            <AdminMessage id={"View billing details"} />
          </a>
        )}
      </div>
    </section>
  )
}

/** A ban or account deletion never cancels the customer's subscriptions. */
export function billingConfirmWarnings(customer: {
  subscriptions: readonly { status: string }[]
}): string[] {
  return customer.subscriptions.some(
    (subscription) => subscription.status !== "canceled"
  )
    ? [
        "This customer has ongoing subscriptions. Banning or deleting the account does not cancel them; cancel them separately in billing.",
      ]
    : []
}

export interface BillingConfirmWarningsOptions extends Connection {
  customer: string
  signal?: AbortSignal
}

const uncheckedBillingWarning =
  "Subscription status could not be fully checked. Banning or deleting the account does not cancel subscriptions; review billing separately."

/** Advisory-only billing check before a host bans or deletes an account. */
export async function loadBillingConfirmWarnings(
  options: BillingConfirmWarningsOptions
): Promise<string[]> {
  const merchant =
    typeof options.merchant === "string"
      ? { id: options.merchant, slug: options.merchant }
      : options.merchant
  const api = createAdminAPI(createAdminClient({ ...options, merchant }))
  let cursor: string | undefined
  const visited = new Set<string>()
  try {
    // This is a confirmation hint, not an unbounded account export. A partial
    // scan stays advisory rather than preventing the host's account action.
    for (let page = 0; page < 10; page++) {
      options.signal?.throwIfAborted()
      const subscriptions = await api.listSubscriptions(
        { customer_id: options.customer },
        100,
        cursor,
        options.signal
      )
      const warnings = billingConfirmWarnings({
        subscriptions: subscriptions.data,
      })
      if (warnings.length) return warnings
      if (!subscriptions.next_cursor) return []
      if (visited.has(subscriptions.next_cursor)) break
      visited.add(subscriptions.next_cursor)
      cursor = subscriptions.next_cursor
    }
  } catch (error) {
    options.signal?.throwIfAborted()
    if (error instanceof Error && error.name === "AbortError") throw error
  }
  return [uncheckedBillingWarning]
}

const permissionAreas: Record<string, string> = {
  "customer:read": "Read customers, subscriptions, payments, and orders.",
  "customer:manage":
    "Change customer billing, including refunds, cancellations, and credits.",
  "catalog:manage": "Change products, prices, meters, and catalog migrations.",
  "catalog:read": "Read products, prices, meters, and catalog migrations.",
  "config:read":
    "Read merchant configuration, PSPs, findings, and notifications.",
  "config:manage":
    "Change merchant configuration, PSPs, findings, and notifications.",
  "metrics:read": "Read revenue metrics, reports, and dashboards.",
  "entitlements:read":
    "Read customer entitlements through the application API.",
  "usage:read":
    "Read usage, admissions, and running charges through the application API.",
  "usage:manage": "Admit and charge usage through the application API.",
  "events:read":
    "Read retained billing events through the application API.",
  "orders:manage":
    "Preview, create, and withdraw merchant orders through the application API.",
  "customers:read":
    "Read merchant customer identity and billing settings.",
}

/** Descriptions for a host's role editor; permissions are enforced by the API. */
export const billingPermissionHelp: Readonly<Record<string, string>> =
  Object.fromEntries(
    ["merchant", "root"].flatMap((scope) =>
      Object.entries(permissionAreas).map(([permission, description]) => [
        `${scope}:${permission}`,
        `${description} ${scope === "root" ? "Applies across merchants." : "Applies within this merchant."}`,
      ])
    )
  )
