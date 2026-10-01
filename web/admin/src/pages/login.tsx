import { LoginForm } from "@openrails/auth-ui"
import { Navigate, useNavigate } from "react-router-dom"

import { LogoLockup } from "@/components/logo"
import { useAuth } from "@/lib/auth"

// Sign-in is AuthKit's own form: password, provider sign-in, second factors,
// account recovery and backup codes.
export function LoginPage() {
  const { ready, signedIn } = useAuth()
  const navigate = useNavigate()
  if (!ready) {
    return (
      <div className="flex min-h-svh items-center justify-center text-sm text-muted-foreground">
        Loading…
      </div>
    )
  }
  if (signedIn) return <Navigate to="/" replace />
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
        <LoginForm
          className="mt-8"
          onSignedIn={() => navigate("/", { replace: true })}
        />
      </div>
    </div>
  )
}
