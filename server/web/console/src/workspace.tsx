import { useState, type ReactNode } from "react"
import { Link, Route, Routes, useLocation, useNavigate } from "react-router-dom"
import { AccountSecurity, StepUpProvider } from "@openrails/auth-ui"
import { AuthUiProvider } from "@openrails/auth-ui/provider"
import {
  useGroupAdminAccess,
  type GroupAdminAccess,
} from "@openrails/auth-ui/admin"
import {
  BillingAdmin,
  useBillingAdminAccess,
  type BillingAdminMerchant,
} from "@openrails/billing-ui/admin"
import type { ConsoleClient } from "./lib/session"
import { useIdentity } from "./lib/identity"
import { useMerchantDirectory } from "./lib/directory"
import { AccessPage } from "./pages/access"
import { takeMerchantFromHash } from "./lib/merchant"
import { Redirect, useRedirect } from "./lib/redirect"
import type { BootstrapConfig } from "./config"

const requestedMerchant = takeMerchantFromHash()

export function MerchantWorkspace({
  config,
  session,
  subject,
}: {
  config: BootstrapConfig
  session: ConsoleClient
  subject: string
}) {
  const directory = useMerchantDirectory(session, subject)
  const [selected, setSelected] = useState(
    () =>
      requestedMerchant ??
      sessionStorage.getItem(`openrails.console.merchant.${subject}`) ??
      ""
  )
  const merchant =
    directory.data?.merchants.find((item) => item.id === selected) ??
    directory.data?.merchants[0]
  const selectMerchant = (id: string) => {
    sessionStorage.setItem(`openrails.console.merchant.${subject}`, id)
    setSelected(id)
  }
  const props = {
    config,
    session,
    merchant,
    merchants: directory.data?.merchants ?? [],
    root: directory.data?.root ?? false,
    selectMerchant,
  }
  return (
    <>
      {directory.data?.error && (
        <div role="alert" className="console-empty">
          {directory.data.error.message}{" "}
          <button onClick={directory.retry}>Try again</button>
        </div>
      )}
      <BillingCapability {...props}>
        {(billing, pending) =>
          session.kind === "local" ? (
            <LocalWorkspace
              {...props}
              session={session}
              billing={billing}
              loading={directory.pending || pending}
            />
          ) : (
            <Workspace
              {...props}
              billing={billing}
              loading={directory.pending || pending}
            />
          )
        }
      </BillingCapability>
    </>
  )
}

interface WorkspaceProps {
  config: BootstrapConfig
  session: ConsoleClient
  merchant?: BillingAdminMerchant
  merchants: BillingAdminMerchant[]
  root: boolean
  selectMerchant(id: string): void
  billing: boolean
  loading?: boolean
  access?: GroupAdminAccess
  group?: string
  setGroup?: (group: string) => void
}

function BillingCapability(
  props: Omit<WorkspaceProps, "billing"> & {
    children: (billing: boolean, pending: boolean) => ReactNode
  }
) {
  if (!props.merchant) return props.children(false, false)
  return <ReadBillingCapability {...props} merchant={props.merchant} />
}

function ReadBillingCapability({
  config,
  session,
  merchant,
  children,
}: Omit<WorkspaceProps, "billing"> & {
  merchant: BillingAdminMerchant
  children: (billing: boolean, pending: boolean) => ReactNode
}) {
  const access = useBillingAdminAccess({
    baseUrl: config.api_base_url,
    auth: session.client,
    merchant,
    exchange:
      session.kind === "local" ? (config.exchange ?? undefined) : undefined,
  })
  const data = access.data
  return (
    <>
      {access.isPending && (
        <p role="status" className="console-empty">
          Loading billing permissions…
        </p>
      )}
      {access.error && (
        <div role="alert" className="console-empty">
          {access.error.message}{" "}
          <button onClick={access.refetch}>Try again</button>
        </div>
      )}
      {children(
        !!data &&
          (data.admin === "read" ||
            data.admin === "update" ||
            data.catalog === "read" ||
            data.catalog === "update" ||
            data.merchant_config === "read" ||
            data.merchant_config === "update" ||
            data.metrics === true),
        access.isPending
      )}
    </>
  )
}

function LocalWorkspace(
  props: WorkspaceProps & { session: Extract<ConsoleClient, { kind: "local" }> }
) {
  const [rootGroup, setRootGroup] = useState<boolean>()
  const group =
    (rootGroup ?? props.root) && props.root
      ? "root"
      : (props.merchant?.id ?? "root")
  const access = useGroupAdminAccess({
    auth: props.session.client,
    baseUrl: props.config.auth_base_url,
    group,
  })
  return (
    <Workspace
      {...props}
      access={access}
      group={group}
      setGroup={(value) => setRootGroup(value === "root")}
    />
  )
}

function Workspace({
  config,
  session,
  merchant,
  merchants,
  root,
  selectMerchant,
  billing,
  loading,
  access,
  group,
  setGroup,
}: WorkspaceProps) {
  const identity = useIdentity()
  const location = useLocation(),
    navigate = useNavigate(),
    redirect = useRedirect()
  const [theme, setTheme] = useState<"light" | "dark">("light")
  const navigation = {
    pathname: "/admin" + location.pathname,
    search: location.search,
    navigate(to: string, options?: { replace?: boolean }) {
      const path = to.slice("/admin".length) || "/"
      if (options?.replace) redirect(path)
      else void navigate(path)
    },
  }
  const hasAccess = session.kind === "issuer" || !!access?.any
  const initial = billing ? "/billing" : hasAccess ? "/access" : "/account"
  return (
    <AuthUiProvider appearance={{ theme }}>
      <NativeStepUp enabled={session.kind === "local"}>
        <div
          className={`console-shell ${theme === "dark" ? "dark" : ""}`}
          data-theme={theme}
        >
          <header className="console-header">
            <Link to="/">OpenRails</Link>
            {merchants.length > 0 && (
              <label>
                Merchant{" "}
                <select
                  value={merchant?.id ?? ""}
                  onChange={(event) => selectMerchant(event.target.value)}
                >
                  {merchants.map((item) => (
                    <option key={item.id} value={item.id}>
                      {item.display_name || item.slug}
                    </option>
                  ))}
                </select>
              </label>
            )}
            <button
              onClick={() =>
                setTheme((value) => (value === "light" ? "dark" : "light"))
              }
            >
              {theme === "light" ? "Dark" : "Light"} theme
            </button>
            <button onClick={() => void identity.signOut()}>Sign out</button>
          </header>
          <nav className="console-tabs" aria-label="Console">
            {billing && <Link to="/billing">Billing</Link>}
            {hasAccess && <Link to="/access">Access</Link>}
            <Link to="/account">Account</Link>
          </nav>
          <Routes>
            <Route
              index
              element={
                loading || access?.status === "loading" ? (
                  <p role="status" className="console-empty">
                    Loading console access…
                  </p>
                ) : (
                  <Redirect to={initial} />
                )
              }
            />
            <Route
              path="billing/*"
              element={
                billing && merchant ? (
                  <BillingAdmin
                    baseUrl={config.api_base_url}
                    auth={session.client}
                    merchant={merchant}
                    merchants={merchants}
                    onMerchantChange={(item) => selectMerchant(item.id)}
                    basePath="/admin/billing"
                    navigation={navigation}
                    exchange={
                      session.kind === "local"
                        ? (config.exchange ?? undefined)
                        : undefined
                    }
                    appearance={{ theme: "inherit" }}
                  />
                ) : (
                  <p className="console-empty">
                    No billing access. Choose a merchant where you hold a
                    billing role.
                  </p>
                )
              }
            />
            <Route
              path="access/*"
              element={
                session.kind === "issuer" ? (
                  <section className="console-section">
                    <h1>Access</h1>
                    <p>
                      People and roles are managed in {session.issuer.name}.
                      Local owners or the operator manage this server's API keys
                      and signing keys.
                    </p>
                  </section>
                ) : (
                  <>
                    {root && (
                      <label className="console-scope">
                        Manage access for{" "}
                        <select
                          value={group}
                          onChange={(event) => setGroup?.(event.target.value)}
                        >
                          {merchant && (
                            <option value={merchant.id}>
                              {merchant.display_name || merchant.slug}
                            </option>
                          )}
                          <option value="root">All-merchants staff</option>
                        </select>
                      </label>
                    )}
                    {access && group && (
                      <AccessPage
                        auth={session.client}
                        baseUrl={config.auth_base_url}
                        group={group}
                        access={access}
                        navigation={navigation}
                      />
                    )}
                  </>
                )
              }
            />
            <Route
              path="account"
              element={
                <section className="console-section">
                  <h1>Account security</h1>
                  {session.kind === "local" ? (
                    <AccountSecurity
                      sections={[
                        "password",
                        "twoFactor",
                        "signInKeys",
                        "sessions",
                      ]}
                    />
                  ) : (
                    <p>
                      Your account and sign-in security are managed in{" "}
                      {session.issuer.name}.
                    </p>
                  )}
                </section>
              }
            />
            <Route
              path="*"
              element={
                <p className="console-empty">
                  That console page does not exist.{" "}
                  <Link to="/">Open the console</Link>
                </p>
              }
            />
          </Routes>
        </div>
      </NativeStepUp>
    </AuthUiProvider>
  )
}

function NativeStepUp({
  enabled,
  children,
}: {
  enabled: boolean
  children: ReactNode
}) {
  return enabled ? <StepUpProvider>{children}</StepUpProvider> : children
}
