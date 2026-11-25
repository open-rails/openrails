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
      <button onClick={() => choose("local")}>
        Sign in with an OpenRails account
      </button>
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
      (config.issuers.length === 0 ? "local" : "")
  )
  const selectedMethod = nativeLink ? "local" : method
  const issuer = config.issuers.find((item) => item.key === selectedMethod)
  const session = useMemo(
    () =>
      issuer || selectedMethod === "local"
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
