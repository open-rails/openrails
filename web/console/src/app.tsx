import { useMemo, useState } from "react"
import { Route, Routes, useLocation, useNavigate } from "react-router-dom"
import {
  ConsoleSession,
  createConsoleSession,
  type ConsoleClient,
} from "./lib/session"
import { ISSUER_REDIRECT_KEY, useIdentity } from "./lib/identity"
import { Redirect } from "./lib/redirect"
import { LoginPage } from "./pages/login"
import { CallbackPage } from "./pages/callback"
import { AccountLinkPage, InvitationPage } from "./pages/account-links"
import { MerchantWorkspace } from "./workspace"
import type { BootstrapConfig } from "./config"

const METHOD_KEY = "openrails.console.sign-in"

function SignInChoices({
  config,
  choose,
}: {
  config: BootstrapConfig
  choose: (key: string) => void
}) {
  return (
    <main className="console-sign-in">
      <h1>Sign in to OpenRails</h1>
      {config.issuers.map((issuer) => (
        <button key={issuer.key} onClick={() => choose(issuer.key)}>
          Sign in with {issuer.name}
        </button>
      ))}
      {config.local_sign_in && (
        <button onClick={() => choose("local")}>
          Sign in with a local account
        </button>
      )}
      {!config.local_sign_in && config.issuers.length === 0 && (
        <div role="status">
          <h2>Declare a sign-in method</h2>
          <p>
            Configure <code>admin_console.issuers</code> to sign in through a
            merchant's app, or enable <code>local_sign_in</code> and seed the
            first operator through the authority manifest.
          </p>
        </div>
      )}
    </main>
  )
}

export function ConsoleApp({ config }: { config: BootstrapConfig }) {
  const location = useLocation()
  const nativeLink = ["/invite", "/reset", "/verify", "/passwordless"].includes(
    location.pathname
  )
  const [method, setMethod] = useState(
    () =>
      sessionStorage.getItem(METHOD_KEY) ??
      (config.issuers.length === 0 && config.local_sign_in ? "local" : "")
  )
  const selectedMethod = nativeLink && config.local_sign_in ? "local" : method
  const issuer = config.issuers.find((item) => item.key === selectedMethod)
  const session = useMemo(
    () =>
      issuer || (selectedMethod === "local" && config.local_sign_in)
        ? createConsoleSession(config, issuer)
        : null,
    [config, issuer, selectedMethod]
  )
  const navigate = useNavigate()
  const choose = (key: string, target?: string) => {
    sessionStorage.removeItem(ISSUER_REDIRECT_KEY)
    sessionStorage.setItem(METHOD_KEY, key)
    setMethod(key)
    const login =
      location.pathname === "/login"
        ? "/login" + location.search
        : `/login?return_to=${encodeURIComponent(location.pathname + location.search)}`
    void navigate(target ?? login)
  }
  if (nativeLink && !config.local_sign_in)
    return (
      <main className="console-sign-in">
        <h1>Local enrollment is unavailable</h1>
        <p>
          This server uses app sign-in. Ask the operator for access through your
          app.
        </p>
      </main>
    )
  if (!session) return <SignInChoices config={config} choose={choose} />
  return (
    <ConsoleSession key={selectedMethod} session={session}>
      <Routes>
        <Route
          path="/login"
          element={
            <>
              <LoginPage />
              <button
                className="console-other-sign-in"
                onClick={() => choose("")}
              >
                Other sign-in options
              </button>
            </>
          }
        />
        {session.kind === "issuer" && (
          <Route path="/callback" element={<CallbackPage />} />
        )}
        {session.kind === "local" && (
          <>
            <Route
              path="/invite"
              element={
                <InvitationPage
                  auth={session.client}
                  onDone={() => choose("local", "/")}
                />
              }
            />
            {(["reset", "verify", "passwordless"] as const).map((kind) => (
              <Route
                key={kind}
                path={`/${kind}`}
                element={
                  <AccountLinkPage
                    auth={session.client}
                    kind={kind}
                    navigate={(to) => void navigate(to)}
                    onDone={(to) => choose("local", to ?? "/")}
                  />
                }
              />
            ))}
          </>
        )}
        <Route
          path="*"
          element={<SignedIn config={config} session={session} />}
        />
      </Routes>
    </ConsoleSession>
  )
}

function SignedIn({
  config,
  session,
}: {
  config: BootstrapConfig
  session: ConsoleClient
}) {
  const identity = useIdentity()
  const location = useLocation()
  if (identity.status === "loading" || identity.status === "restoring")
    return (
      <p role="status" className="console-empty">
        Restoring your session…
      </p>
    )
  if (identity.status !== "signed_in")
    return (
      <Redirect
        to={`/login?return_to=${encodeURIComponent(location.pathname + location.search)}`}
      />
    )
  if (!identity.user)
    return (
      <p role="status" className="console-empty">
        Loading your account…
      </p>
    )
  return (
    <MerchantWorkspace
      key={identity.user.id}
      config={config}
      session={session}
      subject={identity.user.id}
    />
  )
}
