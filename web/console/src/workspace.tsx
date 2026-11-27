import { useState, type ReactNode } from "react"
import { Link, Route, Routes, useLocation, useNavigate } from "react-router-dom"
import { AccountSecurity, StepUpProvider } from "@openrails/auth-ui"
import { AuthUiProvider } from "@openrails/auth-ui/provider"
import { useGroupAdminAccess } from "@openrails/auth-ui/admin"
import {
  BillingAdmin,
  useBillingAdminAccess,
  type BillingAdminMerchant,
} from "@openrails/billing-ui/admin"
import type { AuthClient } from "@openrails/auth-ui/client"
import { useIdentity } from "./lib/identity"
import { useMerchantDirectory } from "./lib/directory"
import { AccessPage } from "./pages/access"
import { NewMerchantPage } from "./pages/new-merchant"
import { TeamPage } from "./pages/team"
import { useOwnAccounts } from "./lib/own-accounts"
import { takeMerchantFromHash } from "./lib/merchant"
import { Redirect, useRedirect } from "./lib/redirect"
import type { BootstrapConfig } from "./config"

const requestedMerchant = takeMerchantFromHash()

export function MerchantWorkspace({
  config,
  auth,
  subject,
}: {
  config: BootstrapConfig
  auth: AuthClient
  subject: string
}) {
  const directory = useMerchantDirectory(auth, subject)
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
    auth,
    merchant,
    merchants: directory.data?.merchants ?? [],
    root: directory.data?.root ?? false,
    selectMerchant,
    // A merchant just created: read the directory again and act on it.
    merchantCreated(created: BillingAdminMerchant) {
      selectMerchant(created.id)
      directory.retry()
    },
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
        {(billing, pending) => (
          <Workspace
            {...props}
            billing={billing}
            loading={directory.pending || pending}
          />
        )}
      </BillingCapability>
    </>
  )
}

interface WorkspaceProps {
  config: BootstrapConfig
  auth: AuthClient
  merchant?: BillingAdminMerchant
  merchants: BillingAdminMerchant[]
  root: boolean
  selectMerchant(id: string): void
  merchantCreated(merchant: BillingAdminMerchant): void
  billing: boolean
  loading?: boolean
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
  auth,
  merchant,
  children,
}: Omit<WorkspaceProps, "billing"> & {
  merchant: BillingAdminMerchant
  children: (billing: boolean, pending: boolean) => ReactNode
}) {
  const access = useBillingAdminAccess({
    baseUrl: config.api_base_url,
    auth,
    merchant,
    exchange: config.exchange ?? undefined,
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

function Workspace({
  config,
  auth,
  merchant,
  merchants,
  root,
  selectMerchant,
  merchantCreated,
  billing,
  loading,
}: WorkspaceProps) {
  const identity = useIdentity()
  const [rootGroup, setRootGroup] = useState<boolean>()
  const group = (rootGroup ?? root) && root ? "root" : (merchant?.id ?? "root")
  const setGroup = (value: string) => setRootGroup(value === "root")
  const access = useGroupAdminAccess({
    auth,
    baseUrl: config.auth_base_url,
    group,
  })
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
  const hasAccess = !!access?.any
  // Operators manage operators where the server runs its own accounts.
  const ownAccounts = useOwnAccounts()
  const team = root && ownAccounts.team
  const initial = billing ? "/billing" : hasAccess ? "/access" : "/account"
  return (
    <AuthUiProvider appearance={{ theme }}>
      <StepUpProvider>
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
            {root && <Link to="/merchants/new">New merchant</Link>}
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
            {team && <Link to="/team">Team</Link>}
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
                loading ? (
                  <p role="status" className="console-empty">
                    Loading billing…
                  </p>
                ) : billing && merchant ? (
                  <BillingAdmin
                    baseUrl={config.api_base_url}
                    auth={auth}
                    merchant={merchant}
                    merchants={merchants}
                    onMerchantChange={(item) => selectMerchant(item.id)}
                    basePath="/admin/billing"
                    navigation={navigation}
                    exchange={config.exchange ?? undefined}
                    newMerchantHref={root ? "/admin/merchants/new" : undefined}
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
                <>
                  {root && (
                    <label className="console-scope">
                      Manage access for{" "}
                      <select
                        value={group}
                        onChange={(event) => setGroup(event.target.value)}
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
                  {access && (
                    <AccessPage
                      auth={auth}
                      baseUrl={config.auth_base_url}
                      group={group}
                      access={access}
                      navigation={navigation}
                    />
                  )}
                </>
              }
            />
            <Route
              path="team/*"
              element={
                team ? (
                  <TeamPage
                    auth={auth}
                    baseUrl={config.auth_base_url}
                    navigation={navigation}
                  />
                ) : (
                  <p className="console-empty">No access to the team.</p>
                )
              }
            />
            <Route
              path="merchants/new"
              element={
                loading ? (
                  <p role="status" className="console-empty">
                    Loading the merchant directory…
                  </p>
                ) : (
                  <NewMerchantPage
                    auth={auth}
                    apiBaseURL={config.api_base_url}
                    root={root}
                    onCreated={merchantCreated}
                  />
                )
              }
            />
            {/* The first run's second step, after the owner's setup: only
                while the directory is empty. */}
            <Route
              path="setup/merchant"
              element={
                loading ? (
                  <p role="status" className="console-empty">
                    Loading the merchant directory…
                  </p>
                ) : root && merchants.length === 0 ? (
                  <NewMerchantPage
                    auth={auth}
                    apiBaseURL={config.api_base_url}
                    root={root}
                    firstRun
                    onCreated={merchantCreated}
                  />
                ) : (
                  <Redirect to="/" />
                )
              }
            />
            <Route
              path="account"
              element={
                <section className="console-section">
                  <h1>Account security</h1>
                  <AccountSecurity
                    sections={[
                      "password",
                      "twoFactor",
                      "signInKeys",
                      "sessions",
                    ]}
                  />
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
      </StepUpProvider>
    </AuthUiProvider>
  )
}
