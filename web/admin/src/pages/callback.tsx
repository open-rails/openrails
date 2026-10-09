import * as React from "react"
import { useIssuerClient } from "@openrails/auth-ui/react"
import { useNavigate } from "react-router-dom"

import { LogoLockup } from "@/components/logo"
import { Button } from "@/components/ui/button"

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
        if (active && result?.kind === "signed_in")
          navigate(result.returnTo ?? "/", { replace: true })
      },
      (reason: unknown) => {
        if (active)
          setError(reason instanceof Error ? reason.message : String(reason))
      }
    )
    return () => {
      active = false
    }
  }, [client, navigate])
  return (
    <div className="flex min-h-svh items-center justify-center bg-background px-6 py-12">
      <div className="w-full max-w-sm">
        <LogoLockup className="h-4" />
        {error ? (
          <>
            <h1 className="mt-8 text-2xl font-semibold tracking-tight">
              Sign-in did not complete
            </h1>
            <p className="mt-2 text-sm text-muted-foreground">{error}</p>
            <Button className="mt-8" onClick={() => navigate("/login")}>
              Try again
            </Button>
          </>
        ) : (
          <p role="status" className="mt-8 text-sm text-muted-foreground">
            Signing in…
          </p>
        )}
      </div>
    </div>
  )
}
