import * as React from "react"
import { LoginForm } from "@openrails/auth-ui"
import { Navigate, useNavigate } from "react-router-dom"

import { LogoLockup } from "@/components/logo"
import { Button } from "@/components/ui/button"
import { useAuth } from "@/lib/auth"
import { ISSUER_REDIRECT_KEY, useIdentity } from "@/lib/identity"

// Sign-in is AuthKit's own form or, with a trusted issuer, a redirect there.
// A tab re-authorizes at the issuer silently (prompt=none) once; after that,
// or when the issuer needs the user, the button does.
export function LoginPage() {
  const { ready, signedIn } = useAuth()
  const { issuer } = useIdentity()
  const navigate = useNavigate()
  const signInAtIssuer = issuer?.signIn
  React.useEffect(() => {
    if (!ready || signedIn || !signInAtIssuer) return
    if (sessionStorage.getItem(ISSUER_REDIRECT_KEY)) return
    sessionStorage.setItem(ISSUER_REDIRECT_KEY, "1")
    void signInAtIssuer("/", true)
  }, [ready, signedIn, signInAtIssuer])
  if (!ready) {
    return (
      <div className="flex min-h-svh items-center justify-center text-sm text-muted-foreground">
        Loading…
      </div>
    )
  }
  if (signedIn) {
    sessionStorage.removeItem(ISSUER_REDIRECT_KEY)
    return <Navigate to="/" replace />
  }
  return (
    <div className="flex min-h-svh items-center justify-center bg-background px-6 py-12">
      <div className="w-full max-w-sm">
        <LogoLockup className="h-4" />
        <h1 className="mt-8 text-2xl font-semibold tracking-tight text-balance">
          Sign in to your console
        </h1>
        <p className="mt-2 text-sm text-pretty text-muted-foreground">
          The merchant console for your OpenRails deployment.
        </p>
        {issuer ? (
          <Button
            className="mt-8 w-full"
            onClick={() => void issuer.signIn("/")}
          >
            Sign in with {issuer.name}
          </Button>
        ) : (
          <LoginForm
            className="mt-8"
            onSignedIn={() => navigate("/", { replace: true })}
          />
        )}
      </div>
    </div>
  )
}
