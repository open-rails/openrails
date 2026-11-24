import { useEffect, type ReactNode } from "react"
import {
  SignInPanel,
  StepUpProvider,
  useStepUpGuard,
  type StepUpProviderProps,
} from "@openrails/auth-ui"
import { useAuthClient, useSession } from "@openrails/auth-ui/react"
import {
  AuthAdmin,
  useGroupAdminAccess,
  useHistoryNavigation,
  useUsersAdminAccess,
  type AdminNavigation,
  type AuthAdminProps,
} from "@openrails/auth-ui/admin"
import { appearanceStyle } from "../appearance"
import { BillingAdmin } from "./billing-admin"
import {
  BillingCustomerSummary,
  loadBillingConfirmWarnings,
  useBillingAdminAccess,
} from "./summary"
import type { BillingAdminMerchant, BillingAdminProps } from "./types"
import { useAdminTheme } from "./theme"

export interface AdminConsoleProps {
  /** AuthKit's JSON API mount, e.g. `/api/v1`. */
  authBaseUrl: string
  /** The engine's API prefix, including /v1. */
  billingBaseUrl: string
  merchant?: string | BillingAdminMerchant
  /** Where the console is mounted. Default `/admin`. */
  basePath?: string
  /** The host router; the history API without one. */
  navigation?: AdminNavigation
  /** The host's sign-in page, opened with `?return_to=`; AuthKit's sign-in panel without one. */
  signInPath?: string
  appearance?: BillingAdminProps["appearance"]
  /** AuthKit's admin language; billing administration is English. */
  language?: string
  /** The user directory's options: tenant, entitlements, languages, and the host's own panels and warnings after billing's. */
  users?: Exclude<AuthAdminProps["users"], false | undefined>
  /** Resolves the linked Solana wallet's signer for a wallet step-up. */
  acquireSolanaSigner?: StepUpProviderProps["acquireSolanaSigner"]
  className?: string
}

/**
 * The staff console: AuthKit's account administration at `basePath` (users,
 * members, roles, credentials) and billing administration at
 * `basePath/billing`, each shown only to whoever holds its permissions. Users
 * carry their billing summary and a subscription warning before a ban or
 * delete; AuthKit's writes step up through the console's dialog. Render it
 * inside the host's `AuthProvider`. Every request is authorized by its API.
 */
export function AdminConsole(props: AdminConsoleProps) {
  const session = useSession()
  const history = useHistoryNavigation()
  const navigation = props.navigation ?? history
  const theme = useAdminTheme(props.appearance?.theme)
  let body: ReactNode
  if (session.status === "loading") body = <Status>Loading…</Status>
  else if (session.status !== "authenticated")
    body = props.signInPath ? (
      <SignInRedirect path={props.signInPath} navigation={navigation} />
    ) : (
      <div
        style={{
          display: "flex",
          justifyContent: "center",
          padding: "2.5rem 0",
        }}
      >
        <SignInPanel />
      </div>
    )
  else
    body = (
      <StepUpProvider
        key={session.userId}
        acquireSolanaSigner={props.acquireSolanaSigner}
      >
        <Console {...props} navigation={navigation} theme={theme} />
      </StepUpProvider>
    )
  return (
    <div className={props.className} data-openrails-admin-console="">
      {body}
    </div>
  )
}

function SignInRedirect({
  path,
  navigation,
}: {
  path: string
  navigation: AdminNavigation
}) {
  const here = navigation.pathname + navigation.search
  useEffect(() => {
    // A full load: the sign-in page is the host's, outside this console's router.
    window.location.assign(`${path}?return_to=${encodeURIComponent(here)}`)
  }, [path, here])
  return <Status>Signing in…</Status>
}

const trimSlashes = (s: string) => s.replace(/^\/+|\/+$/g, "")

function Console({
  authBaseUrl,
  billingBaseUrl,
  merchant,
  basePath = "/admin",
  navigation,
  appearance,
  language,
  users: host = {},
  theme,
}: AdminConsoleProps & { navigation: AdminNavigation; theme: string }) {
  const auth = useAuthClient()
  const guard = useStepUpGuard()
  const users = useUsersAdminAccess({
    auth,
    baseUrl: authBaseUrl,
    tenantId: host.tenantId,
  })
  const root = useGroupAdminAccess({
    auth,
    baseUrl: authBaseUrl,
    group: "root",
  })
  const billing = useBillingAdminAccess({
    baseUrl: billingBaseUrl,
    auth,
    merchant,
  })
  const base = `/${trimSlashes(basePath)}`
  const inside = navigation.pathname.startsWith(`${base}/`)
    ? trimSlashes(navigation.pathname.slice(base.length))
    : ""
  const onBilling = inside === "billing" || inside.startsWith("billing/")
  const accounts = users.any || root.any
  const grant = billing.data
  const customers = !!grant && grant.admin !== "none"
  const billable =
    !!grant &&
    (customers ||
      grant.catalog !== "none" ||
      grant.merchant_config !== "none" ||
      grant.metrics)
  const checking =
    users.status === "loading" || root.status === "loading" || billing.isPending
  const landing = !checking && !accounts && billable && !onBilling
  useEffect(() => {
    if (landing) navigation.navigate(`${base}/billing`, { replace: true })
  }, [landing, navigation, base])

  if (checking) return <Status>Loading…</Status>
  if (!accounts && !billable) {
    const failed = users.error ?? root.error ?? billing.error
    return (
      <Status>
        {failed
          ? "Your access could not be checked."
          : "You have no administrative access."}
        {failed && (
          <>
            {" "}
            <button
              type="button"
              className="underline underline-offset-4"
              onClick={() => {
                users.reload()
                root.reload()
                billing.refetch()
              }}
            >
              Try again
            </button>
          </>
        )}
      </Status>
    )
  }
  const connection = { baseUrl: billingBaseUrl, auth, merchant }
  const tabs = [
    accounts && { href: base, label: "Accounts", current: !onBilling },
    billable && {
      href: `${base}/billing`,
      label: "Billing",
      current: onBilling,
    },
  ].filter((tab) => !!tab)
  return (
    <>
      {tabs.length > 1 && (
        <nav
          aria-label="Administration"
          className="orba mb-6 flex gap-1 border-b px-4"
          data-orba-theme={theme}
          style={appearanceStyle(appearance, "orba")}
        >
          {tabs.map((tab) => (
            <a
              key={tab.href}
              href={tab.href}
              aria-current={tab.current ? "page" : undefined}
              onClick={(event) => {
                if (event.metaKey || event.ctrlKey || event.shiftKey) return
                event.preventDefault()
                navigation.navigate(tab.href)
              }}
              className={[
                "-mb-px border-b-2 px-3 py-2 text-sm font-medium",
                tab.current
                  ? "border-primary text-foreground"
                  : "border-transparent text-muted-foreground hover:text-foreground",
              ].join(" ")}
            >
              {tab.label}
            </a>
          ))}
        </nav>
      )}
      {onBilling || !accounts ? (
        billable && (
          <BillingAdmin
            {...connection}
            basePath={`${base}/billing`}
            navigation={navigation}
            userHref={
              users.users.read
                ? (id) => `${base}/users/${encodeURIComponent(id)}`
                : undefined
            }
            appearance={appearance}
          />
        )
      ) : (
        <div style={{ padding: "0 1.5rem 1.5rem" }}>
          <AuthAdmin
            auth={auth}
            baseUrl={authBaseUrl}
            basePath={base}
            navigation={navigation}
            guard={guard}
            appearance={{ theme: appearance?.theme ?? "inherit" }}
            language={language}
            users={
              customers
                ? {
                    ...host,
                    userPanels: (user) => (
                      <>
                        <BillingCustomerSummary
                          {...connection}
                          key={user.id}
                          customer={user.id}
                          appearance={appearance}
                          href={`${base}/billing/customers/${encodeURIComponent(user.id)}`}
                        />
                        {host.userPanels?.(user)}
                      </>
                    ),
                    confirmWarnings: async (user, action) => {
                      const [billed, own] = await Promise.all([
                        loadBillingConfirmWarnings({
                          ...connection,
                          customer: user.id,
                        }),
                        host.confirmWarnings?.(user, action),
                      ])
                      return [...billed, ...(own == null ? [] : [own].flat())]
                    },
                  }
                : host
            }
          />
        </div>
      )}
    </>
  )
}

function Status({ children }: { children: ReactNode }) {
  return (
    <p role="status" style={{ padding: "1.5rem" }}>
      {children}
    </p>
  )
}
