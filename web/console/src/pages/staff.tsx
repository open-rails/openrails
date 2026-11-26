import { useEffect, useState, type ReactNode } from "react"
import { AcceptInvitation, OwnerSetup } from "@openrails/auth-ui"
import { readInvitation, readSetupToken } from "@openrails/auth-ui/client"
import { Link, useNavigate } from "react-router-dom"
import { useOwnAccounts, type OwnAccounts } from "@/lib/own-accounts"

// The link's fragment, read once and then taken off the address bar.
function useFragment<T>(read: (hash: string) => T): T {
  const [value] = useState(() => read(window.location.hash))
  useEffect(() => {
    window.history.replaceState(
      window.history.state,
      "",
      window.location.pathname + window.location.search
    )
  }, [])
  return value
}

// Renders children only where the server runs its own accounts and serves
// the page's routes.
function OwnAccountsOnly({
  serves,
  children,
}: {
  serves: keyof Pick<OwnAccounts, "setup" | "invitations">
  children: ReactNode
}) {
  const own = useOwnAccounts()
  if (own.loading)
    return (
      <p role="status" className="console-empty">
        Loading…
      </p>
    )
  if (own.error)
    return (
      <div role="alert" className="console-empty">
        The sign-in settings could not be loaded.{" "}
        <button onClick={own.retry}>Try again</button>
      </div>
    )
  if (!own[serves])
    return (
      <p className="console-empty">
        This deployment's accounts are not managed here.{" "}
        <Link to="/">Open the console</Link>
      </p>
    )
  return children
}

// The first owner's page: the setup link the server logs while no account
// holds a root role, or `openrails setup-link` prints.
export function SetupPage() {
  const token = useFragment(readSetupToken)
  const navigate = useNavigate()
  return (
    <OwnAccountsOnly serves="setup">
      <main className="console-sign-in">
        <OwnerSetup
          token={token}
          navigate={(to) => void navigate(to, { replace: true })}
        />
      </main>
    </OwnAccountsOnly>
  )
}

// An invitation's landing page: sign up with it, or accept it into the
// signed-in account. Where the server sends no email the sign-up is
// immediate and the address stays unverified.
export function InvitePage() {
  const invitation = useFragment(readInvitation)
  const [accepted, setAccepted] = useState(false)
  return (
    <OwnAccountsOnly serves="invitations">
      <main className="console-sign-in">
        <AcceptInvitation
          invitation={invitation}
          providers={[]}
          onAccepted={() => setAccepted(true)}
        />
        {accepted && <Link to="/">Open the console</Link>}
      </main>
    </OwnAccountsOnly>
  )
}
