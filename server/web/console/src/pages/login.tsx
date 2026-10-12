import * as React from "react"
import { SignInPanel } from "@openrails/auth-ui"
import { useSearchParams } from "react-router-dom"

import { ISSUER_REDIRECT_KEY, useIdentity } from "@/lib/identity"
import { Redirect, useRedirect } from "@/lib/redirect"

// Sign-in is AuthKit's own form or, with a trusted issuer, a redirect there.
// A tab re-authorizes at the issuer silently (prompt=none) once; after that,
// or when the issuer needs the user, the button does.
export function LoginPage() {
  const { issuer, status } = useIdentity()
  const signedIn = status === "signed_in"
  const ready = status === "signed_out" || signedIn
  const redirect = useRedirect()
  const [params] = useSearchParams()
  const requested = params.get("return_to") ?? "/"
  const returnTo =
    requested.startsWith("/") &&
    !requested.startsWith("//") &&
    !requested.includes("\\")
      ? requested
      : "/"
  const signInAtIssuer = issuer?.signIn
  React.useEffect(() => {
    if (!ready || signedIn || !signInAtIssuer) return
    if (sessionStorage.getItem(ISSUER_REDIRECT_KEY)) return
    sessionStorage.setItem(ISSUER_REDIRECT_KEY, "1")
    void signInAtIssuer(returnTo, true)
  }, [ready, signedIn, signInAtIssuer, returnTo])
  if (!ready) {
    return <div className="console-sign-in">Loading…</div>
  }
  if (signedIn) {
    sessionStorage.removeItem(ISSUER_REDIRECT_KEY)
    return <Redirect to={returnTo} />
  }
  return (
    <div className="console-sign-in">
      <div className="console-sign-in-content">
        <h1 className="console-heading">Sign in to your console</h1>
        <p className="console-description">
          The merchant console for your OpenRails deployment.
        </p>
        {issuer ? (
          <button
            type="button"
            className="console-sign-in-action"
            onClick={() => void issuer.signIn(returnTo)}
          >
            Sign in with {issuer.name}
          </button>
        ) : (
          <SignInPanel
            providers={[]}
            className="console-sign-in-action"
            onSignedIn={() => redirect(returnTo)}
          />
        )}
      </div>
    </div>
  )
}
