import { SignInPanel } from "@openrails/auth-ui"
import { useSearchParams } from "react-router-dom"

import { useIdentity } from "@/lib/identity"
import { Redirect, useRedirect } from "@/lib/redirect"

// Sign-in is AuthKit's own form: the server's own accounts.
export function LoginPage() {
  const { status } = useIdentity()
  const redirect = useRedirect()
  const [params] = useSearchParams()
  const requested = params.get("return_to") ?? "/"
  const returnTo =
    requested.startsWith("/") &&
    !requested.startsWith("//") &&
    !requested.includes("\\")
      ? requested
      : "/"
  if (status === "signed_in") return <Redirect to={returnTo} />
  if (status !== "signed_out") {
    return <div className="console-sign-in">Loading…</div>
  }
  return (
    <div className="console-sign-in">
      <div className="console-sign-in-content">
        <h1 className="console-heading">Sign in to your console</h1>
        <p className="console-description">
          The merchant console for your OpenRails deployment.
        </p>
        <SignInPanel
          providers={[]}
          className="console-sign-in-action"
          onSignedIn={() => redirect(returnTo)}
        />
      </div>
    </div>
  )
}
