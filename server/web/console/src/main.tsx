import { StrictMode, useMemo, useState } from "react"
import { createRoot } from "react-dom/client"
import {
  BrowserRouter,
  Navigate,
  Route,
  Routes,
  useLocation,
  useNavigate,
} from "react-router-dom"
import { BillingAdmin } from "@openrails/billing-ui/admin"
import "@openrails/billing-ui/admin.css"
import "./style.css"
import {
  ConsoleSession,
  createConsoleSession,
  type ConsoleClient,
} from "./lib/session"
import { useIdentity } from "./lib/identity"
import { bootstrapURL, routerBasename } from "./lib/mount"
import { LoginPage } from "./pages/login"
import { CallbackPage } from "./pages/callback"
import { takeMerchantFromHash } from "./lib/merchant"
import type { BootstrapConfig } from "./config"

const requestedMerchant = takeMerchantFromHash()

function Billing({
  config,
  session,
}: {
  config: BootstrapConfig
  session: ConsoleClient
}) {
  const identity = useIdentity()
  const location = useLocation(),
    navigate = useNavigate()
  const basePath = routerBasename().replace(/\/$/, "")
  const [merchant, setMerchant] = useState(
    () => config.merchant?.id ?? requestedMerchant ?? ""
  )
  const [opened, setOpened] = useState(merchant)
  const navigation = useMemo(
    () => ({
      pathname: basePath + location.pathname,
      search: location.search,
      navigate(to: string, options?: { replace?: boolean }) {
        void navigate(to.slice(basePath.length) || "/", options)
      },
    }),
    [basePath, location.pathname, location.search, navigate]
  )
  if (identity.status === "loading" || identity.status === "restoring")
    return <p role="status">Loading…</p>
  if (identity.status !== "signed_in") return <Navigate to="/login" replace />
  return (
    <>
      <header className="console-header">
        <strong>OpenRails</strong>
        <form
          onSubmit={(event) => {
            event.preventDefault()
            setOpened(merchant.trim())
          }}
        >
          <label>
            Merchant id{" "}
            <input
              value={merchant}
              onChange={(event) => setMerchant(event.target.value)}
              required
            />
          </label>
          <button type="submit">Open</button>
        </form>
        <button type="button" onClick={() => void identity.signOut()}>
          Sign out
        </button>
      </header>
      {opened ? (
        <BillingAdmin
          key={identity.user?.id}
          baseUrl={config.api_base_url}
          auth={session.client}
          merchant={config.merchant ?? opened}
          basePath={basePath}
          navigation={navigation}
          exchange={
            config.exchange
              ? {
                  resource: config.exchange.resource,
                  scope: config.exchange.scope,
                }
              : undefined
          }
          appearance={{ theme: "light" }}
        />
      ) : (
        <p className="console-empty">
          Enter the id of the merchant you work on.
        </p>
      )}
    </>
  )
}

const root = createRoot(document.getElementById("root")!)
fetch(bootstrapURL(), { cache: "no-store" })
  .then(async (response) => {
    if (!response.ok)
      throw new Error(`Console configuration failed (${response.status})`)
    const config = (await response.json()) as BootstrapConfig
    const session = createConsoleSession(config)
    const billing = <Billing config={config} session={session} />
    root.render(
      <StrictMode>
        <ConsoleSession session={session}>
          <BrowserRouter basename={routerBasename()}>
            <Routes>
              <Route path="/login" element={<LoginPage />} />
              {session.kind === "issuer" && (
                <Route path="/callback" element={<CallbackPage />} />
              )}
              <Route path="*" element={billing} />
            </Routes>
          </BrowserRouter>
        </ConsoleSession>
      </StrictMode>
    )
  })
  .catch((error: unknown) =>
    root.render(
      <div role="alert">
        Admin console failed to start:{" "}
        {error instanceof Error ? error.message : String(error)}
      </div>
    )
  )
