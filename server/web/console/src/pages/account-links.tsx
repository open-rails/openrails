import { useEffect, useRef, useState } from "react"
import { ResetPasswordForm, SignInPanel, VerifyLink } from "@openrails/auth-ui"
import { readLinkFragment, type AuthClient } from "@openrails/auth-ui/client"
import { useIdentity } from "../lib/identity"

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

export function InvitationPage({
  auth,
  onDone,
}: {
  auth: AuthClient
  onDone: () => void
}) {
  const params = useLinkParams()
  const accountCode = params.get("invite_code") ?? undefined
  const code = accountCode ?? params.get("code") ?? ""
  const identity = useIdentity()
  const mode = useRef(accountCode ? "register" : "login")
  const [error, setError] = useState<string>()
  const [pending, setPending] = useState(false)
  async function accept() {
    setPending(true)
    setError(undefined)
    try {
      await auth.redeemInvitation(code)
      onDone()
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason))
    } finally {
      setPending(false)
    }
  }
  if (!code)
    return (
      <p className="console-empty">
        Open the invitation link from its original message.
      </p>
    )
  return (
    <main className="console-sign-in">
      <h1>Join this team</h1>
      {identity.status === "loading" || identity.status === "restoring" ? (
        <p role="status">Restoring your session…</p>
      ) : identity.status === "signed_in" ? (
        <>
          <p>
            Accept this invitation as{" "}
            {identity.user?.email ??
              identity.user?.username ??
              "the signed-in account"}
            .
          </p>
          {error && <p role="alert">{error}</p>}
          <button disabled={pending} onClick={() => void accept()}>
            {pending ? "Accepting…" : "Accept invitation"}
          </button>
          <button disabled={pending} onClick={() => void auth.signOut()}>
            Use another account
          </button>
        </>
      ) : (
        <SignInPanel
          providers={[]}
          inviteCode={accountCode}
          initialTab={accountCode ? "register" : "login"}
          onStepChange={(step) => {
            if (step === "register" || step === "login") mode.current = step
          }}
          onSignedIn={() => {
            if (mode.current === "register") onDone()
          }}
        />
      )}
    </main>
  )
}

export function AccountLinkPage({
  kind,
  navigate,
  onDone,
}: {
  kind: "reset" | "verify" | "passwordless"
  navigate: (to: string) => void
  onDone: (to?: string) => void
}) {
  const [link] = useState(() => readLinkFragment(window.location.hash))
  useLinkParams()
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
