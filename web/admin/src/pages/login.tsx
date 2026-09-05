import { HugeiconsIcon } from "@hugeicons/react"
import { Key02Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { Navigate, useNavigate } from "react-router-dom"
import { useMutation } from "@tanstack/react-query"

import { LogoLockup } from "@/components/logo"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Separator } from "@/components/ui/separator"
import {
  useAuth,
  type TwoFactorChallenge,
  type TwoFactorVerificationMode,
} from "@/lib/auth"
import { authMutations } from "@/lib/auth-mutations"

function factorLabel(method: string, phoneNumber?: string) {
  switch (method) {
    case "email":
      return "Email"
    case "sms":
      return phoneNumber ? `Text message to ${phoneNumber}` : "Text message"
    case "totp":
      return "Authenticator app"
    default:
      return method
  }
}

function verificationPrompt(challenge: TwoFactorChallenge) {
  switch (challenge.method) {
    case "email":
      return challenge.verificationID
        ? `Enter the code sent to ${challenge.verificationID}.`
        : "Enter the code sent to your email."
    case "sms":
      return challenge.verificationID
        ? `Enter the code sent to ${challenge.verificationID}.`
        : "Enter the code sent by text message."
    case "totp":
      return "Open your authenticator app and enter the code it shows."
    default:
      return "Enter the verification code for your selected security method."
  }
}

export function LoginPage() {
  const {
    ready,
    bootError,
    me,
    capabilities,
    loginWithPassword,
    completeTwoFactor,
    selectTwoFactor,
    startOIDC,
  } = useAuth()
  const navigate = useNavigate()
  const [login, setLogin] = React.useState("")
  const [password, setPassword] = React.useState("")
  // Set once the password step succeeds but the account needs a second factor.
  // Its presence is what swaps the form for the code step.
  const [challenge, setChallenge] = React.useState<TwoFactorChallenge | null>(
    null
  )
  const [code, setCode] = React.useState("")
  const [verificationMode, setVerificationMode] =
    React.useState<TwoFactorVerificationMode>("factor")
  const passwordLogin = useMutation(authMutations.login(loginWithPassword))
  const verifyTwoFactor = useMutation(
    authMutations.verifyTwoFactor(completeTwoFactor)
  )
  const factorSelection = useMutation(
    authMutations.selectTwoFactor(selectTwoFactor)
  )
  const failure =
    passwordLogin.error ?? factorSelection.error ?? verifyTwoFactor.error
  const error = failure
    ? failure instanceof Error
      ? failure.message
      : String(failure)
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
      { identifier: login, password },
      {
        onSuccess: (pending) => {
          // A challenge means the password was right and a code is still owed;
          // only a null result is a finished sign-in.
          if (pending) {
            setChallenge(pending)
            setVerificationMode("factor")
            setCode("")
            return
          }
          navigate("/", { replace: true })
        },
      }
    )
  }

  const submitCode = (e: React.FormEvent) => {
    e.preventDefault()
    if (!challenge) return
    verifyTwoFactor.mutate(
      { challenge, code, mode: verificationMode },
      { onSuccess: () => navigate("/", { replace: true }) }
    )
  }

  const chooseFactor = (factorId: string | null) => {
    if (!challenge || !factorId || factorId === challenge.factor.id) return
    factorSelection.mutate(
      { challenge, factorId },
      {
        onSuccess: (nextChallenge) => {
          setChallenge(nextChallenge)
          setCode("")
          verifyTwoFactor.reset()
        },
      }
    )
  }

  const changeVerificationMode = (mode: TwoFactorVerificationMode) => {
    setVerificationMode(mode)
    setCode("")
    factorSelection.reset()
    verifyTwoFactor.reset()
  }

  return (
    <div className="flex min-h-svh items-center justify-center bg-background px-6 py-12">
      <div className="w-full max-w-sm">
        {/* the real lockup, in place of the placeholder glyph that stood here */}
        <LogoLockup className="h-4" />
        <h1 className="mt-8 text-2xl font-semibold tracking-tight text-balance">
          {challenge
            ? verificationMode === "backup_code"
              ? "Enter a backup code"
              : "Enter your verification code"
            : "Sign in to your console"}
        </h1>
        <p className="mt-2 text-sm text-pretty text-muted-foreground">
          {challenge
            ? verificationMode === "backup_code"
              ? "Enter one of the backup codes you saved when setting up two-factor authentication."
              : verificationPrompt(challenge)
            : "The merchant console for your OpenRails deployment."}
        </p>
        <div className="mt-8 grid gap-4">
          {bootError && (
            <p className="text-sm text-destructive" role="alert">
              Console bootstrap failed: {bootError}
            </p>
          )}
          {challenge && (
            <form onSubmit={submitCode} className="grid gap-3">
              {verificationMode === "factor" &&
                challenge.factors.length > 1 && (
                  <div className="grid gap-1.5">
                    <Label htmlFor="factor">Security method</Label>
                    <Select
                      items={challenge.factors.map((factor) => ({
                        value: factor.id,
                        label: factorLabel(factor.method, factor.phone_number),
                      }))}
                      value={challenge.factor.id}
                      onValueChange={chooseFactor}
                      disabled={
                        factorSelection.isPending || verifyTwoFactor.isPending
                      }
                    >
                      <SelectTrigger id="factor" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {challenge.factors.map((factor) => (
                          <SelectItem key={factor.id} value={factor.id}>
                            {factorLabel(factor.method, factor.phone_number)}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                )}
              <div className="grid gap-1.5">
                <Label htmlFor="code">
                  {verificationMode === "backup_code"
                    ? "Backup code"
                    : "Verification code"}
                </Label>
                <Input
                  id="code"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                  inputMode={
                    verificationMode === "backup_code" ? "text" : "numeric"
                  }
                  autoComplete={
                    verificationMode === "backup_code" ? "off" : "one-time-code"
                  }
                  autoFocus
                  required
                />
              </div>
              {error && (
                <p className="text-sm text-destructive" role="alert">
                  {error}
                </p>
              )}
              <Button
                type="submit"
                disabled={
                  factorSelection.isPending || verifyTwoFactor.isPending
                }
              >
                {verifyTwoFactor.isPending ? "Verifying…" : "Verify"}
              </Button>
              <Button
                type="button"
                variant="outline"
                disabled={
                  factorSelection.isPending || verifyTwoFactor.isPending
                }
                onClick={() =>
                  changeVerificationMode(
                    verificationMode === "backup_code"
                      ? "factor"
                      : "backup_code"
                  )
                }
              >
                {verificationMode === "backup_code"
                  ? `Use ${factorLabel(challenge.method).toLowerCase()}`
                  : "Use a backup code"}
              </Button>
              <Button
                type="button"
                variant="ghost"
                onClick={() => {
                  setChallenge(null)
                  setCode("")
                  setPassword("")
                  setVerificationMode("factor")
                  passwordLogin.reset()
                  factorSelection.reset()
                  verifyTwoFactor.reset()
                }}
              >
                Back
              </Button>
            </form>
          )}
          {!challenge && passwordEnabled && (
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
          {!challenge && oidcProviders.length > 0 && (
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
          {!challenge &&
            !passwordEnabled &&
            oidcProviders.length === 0 &&
            !bootError && (
              <p className="text-sm text-muted-foreground">
                The auth issuer advertises no browser login methods. Check the
                deployment's AuthKit configuration.
              </p>
            )}
        </div>
      </div>
    </div>
  )
}
