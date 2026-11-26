import { useMemo } from "react"
import type { AuthClient } from "@openrails/auth-ui/client"
import { Route, Routes, useLocation, useNavigate } from "react-router-dom"
import { ConsoleSession, createConsoleSession } from "./lib/session"
import { useIdentity } from "./lib/identity"
import { Redirect } from "./lib/redirect"
import { LoginPage } from "./pages/login"
import { AccountLinkPage } from "./pages/account-links"
import { InvitePage, SetupPage } from "./pages/staff"
import { MerchantWorkspace } from "./workspace"
import type { BootstrapConfig } from "./config"

export function ConsoleApp({ config }: { config: BootstrapConfig }) {
  const auth = useMemo(() => createConsoleSession(config), [config])
  const navigate = useNavigate()
  return (
    <ConsoleSession auth={auth}>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/setup" element={<SetupPage />} />
        <Route path="/invite" element={<InvitePage />} />
        {(["reset", "verify", "passwordless"] as const).map((kind) => (
          <Route
            key={kind}
            path={`/${kind}`}
            element={
              <AccountLinkPage
                auth={auth}
                kind={kind}
                navigate={(to) => void navigate(to)}
                onDone={(to) => void navigate(to ?? "/")}
              />
            }
          />
        ))}
        <Route path="*" element={<SignedIn config={config} auth={auth} />} />
      </Routes>
    </ConsoleSession>
  )
}

function SignedIn({
  config,
  auth,
}: {
  config: BootstrapConfig
  auth: AuthClient
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
      auth={auth}
      subject={identity.user.id}
    />
  )
}
