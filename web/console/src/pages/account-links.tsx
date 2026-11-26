import { useEffect, useRef, useState } from "react"
import { ResetPasswordForm, SignInPanel, VerifyLink } from "@openrails/auth-ui"
import {
  readLinkFragment,
  type AuthClient,
  type PendingSignIn,
  type SignInResult,
} from "@openrails/auth-ui/client"

function useLinkParams() {
  const [params] = useState(
    () => new URLSearchParams(window.location.hash.slice(1))
  )
  useEffect(() => {
    window.history.replaceState(
      window.history.state,
      "",
      window.location.pathname + window.location.search
    )
  }, [])
  return params
}

export function AccountLinkPage({
  auth,
  kind,
  navigate,
  onDone,
}: {
  auth: AuthClient
  kind: "reset" | "verify" | "passwordless"
  navigate: (to: string) => void
  onDone: (to?: string) => void
}) {
  const [link] = useState(() => readLinkFragment(window.location.hash))
  useLinkParams()
  if (kind === "passwordless")
    return <PasswordlessLink auth={auth} token={link?.token} onDone={onDone} />
  return (
    <main className="console-sign-in">
      {kind === "reset" ? (
        <ResetPasswordForm
          token={link?.token}
          onDone={() => onDone("/login")}
          onRequestNewLink={() => onDone("/login")}
        />
      ) : (
        <VerifyLink
          token={link?.token}
          navigate={navigate}
          fallbackPath="/login"
          onVerified={(result) => onDone(result.signedIn ? "/" : "/account")}
        />
      )}
    </main>
  )
}

function PasswordlessLink({
  auth,
  token,
  onDone,
}: {
  auth: AuthClient
  token?: string | null
  onDone: () => void
}) {
  const confirmation = useRef<Promise<SignInResult>>(undefined)
  const done = useRef(onDone)
  const [continuation, setContinuation] = useState<PendingSignIn>()
  const [error, setError] = useState<string>()
  useEffect(() => {
    done.current = onDone
  })
  useEffect(() => {
    if (!token) return
    let active = true
    // StrictMode may replay the effect; a single-use link is confirmed once.
    confirmation.current ??= auth.confirmPasswordless({ token })
    void confirmation.current.then(
      (result) => {
        if (!active) return
        if (result.status === "complete") done.current()
        else setContinuation(result)
      },
      (reason: unknown) => {
        if (active)
          setError(reason instanceof Error ? reason.message : String(reason))
      }
    )
    return () => {
      active = false
    }
  }, [auth, token])
  return (
    <main className="console-sign-in">
      {!token ? (
        <p>Open the sign-in link from its original message.</p>
      ) : error ? (
        <p role="alert">{error}</p>
      ) : continuation ? (
        <SignInPanel
          providers={[]}
          continuation={continuation}
          onSignedIn={() => onDone()}
        />
      ) : (
        <p role="status">Confirming your sign-in link…</p>
      )}
      {(!token || error) && (
        <button onClick={() => onDone()}>Back to sign in</button>
      )}
    </main>
  )
}
