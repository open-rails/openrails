import { useEffect, useMemo, useState, type FormEvent } from "react"
import {
  matchPath,
  Route,
  Routes,
  useLocation,
  useNavigate,
} from "react-router-dom"
import {
  ConsoleSession,
  consoleURL,
  createConsoleSession,
  type ConsoleClient,
} from "./lib/session"
import { ISSUER_REDIRECT_KEY, useIdentity } from "./lib/identity"
import { Redirect } from "./lib/redirect"
import { LoginPage } from "./pages/login"
import { CallbackPage } from "./pages/callback"
import { AccountLinkPage, InvitationPage } from "./pages/account-links"
import { MerchantWorkspace } from "./workspace"
import type { BootstrapConfig, ConsoleIssuer } from "./config"

const METHOD_KEY = "openrails.console.sign-in"
// A merchant's issuer, found by its name, outlives the redirect to it.
const ISSUER_KEY = "openrails.console.issuer"

// MERCHANT_PATH is a merchant's own sign-in page, for staff to bookmark.
const MERCHANT_PATH = "/m/:merchant"

function storedIssuer(): ConsoleIssuer | undefined {
  const raw = sessionStorage.getItem(ISSUER_KEY)
  if (!raw) return undefined
  try {
    return JSON.parse(raw) as ConsoleIssuer
  } catch {
    return undefined
  }
}

// merchantIssuers are the sign-in buttons of the merchant named name; none
// for a name the server knows no app sign-in for.
async function merchantIssuers(name: string): Promise<ConsoleIssuer[]> {
  const url = new URL(consoleURL("/sign-in"))
  url.searchParams.set("merchant", name)
  const response = await fetch(url.href, { cache: "no-store" })
  if (response.status === 404) return []
  if (!response.ok)
    throw new Error(`App sign-in is unavailable (${response.status})`)
  const body = (await response.json()) as { issuers: ConsoleIssuer[] }
  return body.issuers
}

interface Found {
  issuers?: ConsoleIssuer[]
  error?: string
}

async function lookUp(name: string): Promise<Found> {
  try {
    return { issuers: await merchantIssuers(name) }
  } catch (error: unknown) {
    return { error: error instanceof Error ? error.message : String(error) }
  }
}

function SignInChoices({
  config,
  merchant,
  choose,
}: {
  config: BootstrapConfig
  merchant?: string
  choose: (key: string, issuer?: ConsoleIssuer) => void
}) {
  const [name, setName] = useState(merchant ?? "")
  const [found, setFound] = useState<Found>()
  useEffect(() => {
    if (!merchant || !config.app_sign_in) return
    let live = true
    void lookUp(merchant).then((result) => {
      if (live) setFound(result)
    })
    return () => {
      live = false
    }
  }, [merchant, config.app_sign_in])
  const submit = (event: FormEvent) => {
    event.preventDefault()
    const wanted = name.trim()
    if (wanted) void lookUp(wanted).then(setFound)
  }
  return (
    <main className="console-sign-in">
      <h1>Sign in to OpenRails</h1>
      {found?.issuers?.map((issuer) => (
        <button key={issuer.key} onClick={() => choose(issuer.key, issuer)}>
          Sign in with {issuer.name}
        </button>
      ))}
      {config.issuers.map((issuer) => (
        <button key={issuer.key} onClick={() => choose(issuer.key)}>
          Sign in with {issuer.name}
        </button>
      ))}
      <button onClick={() => choose("local")}>
        Sign in with an OpenRails account
      </button>
      {config.app_sign_in && !found?.issuers?.length && (
        <form onSubmit={submit}>
          <label>
            Sign in with your app: merchant name{" "}
            <input
              value={name}
              onChange={(event) => setName(event.target.value)}
              autoComplete="organization"
            />
          </label>
          <button type="submit" disabled={!name.trim()}>
            Continue
          </button>
        </form>
      )}
      {found && found.issuers?.length === 0 && (
        <p role="status">No app sign-in for that name.</p>
      )}
      {found?.error && <p role="alert">{found.error}</p>}
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
  const issuer =
    config.issuers.find((item) => item.key === selectedMethod) ??
    [storedIssuer()].find((item) => item?.key === selectedMethod)
  const session = useMemo(
    () =>
      issuer || selectedMethod === "local"
        ? createConsoleSession(config, issuer)
        : null,
    [config, issuer, selectedMethod]
  )
  const navigate = useNavigate()
  const merchant = matchPath(MERCHANT_PATH, location.pathname)?.params.merchant
  const choose = (key: string, found?: ConsoleIssuer, target?: string) => {
    sessionStorage.removeItem(ISSUER_REDIRECT_KEY)
    sessionStorage.setItem(METHOD_KEY, key)
    if (found) sessionStorage.setItem(ISSUER_KEY, JSON.stringify(found))
    setMethod(key)
    const login =
      location.pathname === "/login"
        ? "/login" + location.search
        : merchant !== undefined
          ? "/login"
          : `/login?return_to=${encodeURIComponent(location.pathname + location.search)}`
    void navigate(target ?? login)
  }
  if (!session)
    return (
      <SignInChoices
        key={merchant ?? ""}
        config={config}
        merchant={merchant}
        choose={choose}
      />
    )
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
                  onDone={() => choose("local", undefined, "/")}
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
                    onDone={(to) => choose("local", undefined, to ?? "/")}
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
