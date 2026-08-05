import { HugeiconsIcon } from "@hugeicons/react"
import { Key02Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { Navigate, useNavigate } from "react-router-dom"
import { useMutation } from "@tanstack/react-query"

import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Separator } from "@/components/ui/separator"
import { useAuth } from "@/lib/auth"
import { authMutations } from "@/lib/auth-mutations"

export function LoginPage() {
  const { ready, bootError, me, capabilities, loginWithPassword, startOIDC } =
    useAuth()
  const navigate = useNavigate()
  const [login, setLogin] = React.useState("")
  const [password, setPassword] = React.useState("")
  const passwordLogin = useMutation(authMutations.login(loginWithPassword))
  const error = passwordLogin.error
    ? passwordLogin.error instanceof Error
      ? passwordLogin.error.message
      : String(passwordLogin.error)
    : undefined

  if (!ready) {
    return (
      <div className="flex min-h-svh items-center justify-center text-sm text-muted-foreground">
        Loading…
      </div>
    )
  }
  if (me) return <Navigate to="/" replace />

  // OIDC buttons only when the issuer actually advertises login-capable
  // providers; otherwise password-only.
  const oidcProviders = (capabilities?.providers ?? []).filter(
    (p) => p.supports_login
  )
  const passwordEnabled = capabilities?.password?.login !== false

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    passwordLogin.mutate(
      { login, password },
      { onSuccess: () => navigate("/", { replace: true }) }
    )
  }

  return (
    <div className="flex min-h-svh items-center justify-center bg-muted/40 p-4">
      <Card className="w-full max-w-sm">
        <CardHeader className="text-center">
          <div className="mx-auto mb-2 flex size-10 items-center justify-center rounded-lg bg-primary text-sm font-semibold text-primary-foreground">
            µ
          </div>
          <CardTitle>OpenRails Admin</CardTitle>
          <CardDescription>Sign in to your merchant console</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          {bootError && (
            <p className="text-sm text-destructive">
              Console bootstrap failed: {bootError}
            </p>
          )}
          {passwordEnabled && (
            <form onSubmit={submit} className="grid gap-3">
              <div className="grid gap-1.5">
                <Label htmlFor="login">Email or username</Label>
                <Input
                  id="login"
                  value={login}
                  onChange={(e) => setLogin(e.target.value)}
                  autoComplete="username"
                  required
                />
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="password">Password</Label>
                <Input
                  id="password"
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  autoComplete="current-password"
                  required
                />
              </div>
              {error && (
                <p className="text-sm text-destructive" role="alert">
                  {error}
                </p>
              )}
              <Button type="submit" disabled={passwordLogin.isPending}>
                <HugeiconsIcon icon={Key02Icon} className="size-4" />
                {passwordLogin.isPending ? "Signing in…" : "Sign in"}
              </Button>
            </form>
          )}
          {oidcProviders.length > 0 && (
            <>
              {passwordEnabled && (
                <div className="flex items-center gap-2 text-xs text-muted-foreground">
                  <Separator className="flex-1" /> or{" "}
                  <Separator className="flex-1" />
                </div>
              )}
              <div className="grid gap-2">
                {oidcProviders.map((p) => (
                  <Button
                    key={p.id}
                    variant="outline"
                    onClick={() => startOIDC(p.id)}
                  >
                    Continue with {p.name || p.id}
                  </Button>
                ))}
              </div>
            </>
          )}
          {!passwordEnabled && oidcProviders.length === 0 && !bootError && (
            <p className="text-sm text-muted-foreground">
              The auth issuer advertises no browser login methods. Check the
              deployment's AuthKit configuration.
            </p>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
