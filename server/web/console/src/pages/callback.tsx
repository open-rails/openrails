import * as React from "react"
import { isOAuthError } from "@openrails/auth-ui/client"
import { useIssuerClient } from "@openrails/auth-ui/react"
import { useNavigate } from "react-router-dom"

import { ISSUER_REDIRECT_KEY } from "@/lib/identity"

// The trusted issuer returns here with the authorization code; the client
// redeems it and the console continues where sign-in started.
export function CallbackPage() {
  const client = useIssuerClient()
  const navigate = useNavigate()
  const [error, setError] = React.useState<string | null>(null)
  React.useEffect(() => {
    let active = true
    client.completeSignIn().then(
      (result) => {
        if (active && result?.kind === "signed_in") {
          sessionStorage.removeItem(ISSUER_REDIRECT_KEY)
          navigate(result.returnTo ?? "/", { replace: true })
        }
      },
      (reason: unknown) => {
        if (!active) return
        // A silent re-authorization the issuer cannot answer without the
        // user: sign in with the button.
        if (
          isOAuthError(reason) &&
          [
            "login_required",
            "interaction_required",
            "consent_required",
          ].includes(reason.error)
        ) {
          navigate("/login", { replace: true })
          return
        }
        setError(reason instanceof Error ? reason.message : String(reason))
      }
    )
    return () => {
      active = false
    }
  }, [client, navigate])
  return (
    <div className="console-sign-in">
      <div className="console-sign-in-content">
        {error ? (
          <>
            <h1 className="console-heading">Sign-in did not complete</h1>
            <p className="console-description">{error}</p>
            <button
              type="button"
              className="console-sign-in-action"
              onClick={() => navigate("/login")}
            >
              Try again
            </button>
          </>
        ) : (
          <p role="status" className="console-description">
            Signing in…
          </p>
        )}
      </div>
    </div>
  )
}
