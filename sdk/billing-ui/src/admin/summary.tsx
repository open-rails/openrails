import { AdminMessage } from "#orck/admin/ui-context"
import { useCallback, useEffect, useMemo, useState } from "react"
import { appearanceStyle } from "../appearance"
import type { AdminAccess } from "../client/generated/wire"
import { ApiError, createAdminClient } from "./lib/api/client"
import { createAdminAPI } from "./lib/api"
import { formatNativeAmount } from "./lib/format"
import { errorText } from "./lib/error-text"
import { statusLabel } from "./components/status-badge"
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
      <div className="overflow-hidden rounded-xl bg-card text-sm text-card-foreground shadow-xs ring-1 ring-foreground/10">
        <div className="flex flex-wrap items-center justify-between gap-3 border-b px-5 py-5 sm:px-6">
          <h2 className="text-base leading-tight font-semibold">
            <AdminMessage id={"Billing"} />
          </h2>
          {props.href && (
            <a
              className="inline-flex min-h-10 items-center text-sm text-muted-foreground underline-offset-4 hover:text-foreground hover:underline sm:min-h-0"
              href={props.href}
            >
              <AdminMessage id={"View billing details"} />
            </a>
          )}
        </div>
        <div className="px-5 py-4 sm:px-6">
          {isPending && (
            <p role="status" className="text-muted-foreground">
              <AdminMessage id={"Loading billing…"} />
            </p>
          )}
          {error && missingCustomer(error) && (
            <p className="text-muted-foreground">
              <AdminMessage id={"No billing activity yet."} />
            </p>
          )}
          {error && !missingCustomer(error) && (
            <p role="alert">
              {errorText(error).text}{" "}
              <button
                type="button"
                className="underline underline-offset-4"
                onClick={refetch}
              >
                <AdminMessage id={"Try again"} />
              </button>
            </p>
          )}
          {data && (
            <dl className="grid gap-x-6 gap-y-1 sm:grid-cols-[10rem_minmax(0,1fr)] sm:gap-y-3">
              {data.customer.balances.map((balance) => (
                <SummaryRow
                  key={balance.currency}
                  label="Available balance"
                  items={[
                    formatNativeAmount(
                      balance.available_amount,
                      balance.currency
                    ),
                  ]}
                />
              ))}
              <SummaryRow
                label="Active subscriptions"
                items={data.subscriptions.data.map(
                  (subscription) =>
                    subscription.product?.display_name ?? subscription.id
                )}
              />
              <SummaryRow
                label="Entitlements"
                items={data.entitlements.data.map(
                  (entitlement) => entitlement.entitlement
                )}
              />
              <SummaryRow
                label="Product access"
                items={data.access.data.map(
                  (access) =>
                    `${access.product_name} · ${statusLabel(access.status)}`
                )}
              />
              <SummaryRow
                label="Recent payments"
                items={data.payments.data.map(
                  (payment) =>
                    `${formatNativeAmount(payment.amount, payment.currency)} · ${statusLabel(payment.status)}`
                )}
              />
            </dl>
          )}
        </div>
      </div>
    </section>
  )
}

// SummaryRow is one labelled list in the panel; an empty one says "None".
function SummaryRow({ label, items }: { label: string; items: string[] }) {
  return (
    <>
      <dt className="pt-2 text-muted-foreground first:pt-0 sm:pt-0">
        <AdminMessage id={label} />
      </dt>
      <dd className="min-w-0">
        {items.length ? (
          <ul className="grid gap-0.5">
            {items.map((item, index) => (
              <li key={index}>{item}</li>
            ))}
          </ul>
        ) : (
          <span className="text-muted-foreground">
            <AdminMessage id={"None"} />
          </span>
        )}
      </dd>
    </>
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
