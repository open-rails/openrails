import { useEffect, useState, type FormEvent } from "react"
import { Link, useNavigate } from "react-router-dom"
import type { AuthClient } from "@openrails/auth-ui/client"
import type { BillingAdminMerchant } from "@openrails/billing-ui/admin"
import { consoleURL } from "@/lib/session"

// useConfigurationEdits reports whether merchant configuration can change
// here (Vault holds it): only then does a merchant take a display name from
// the console. Undefined while it loads.
function useConfigurationEdits(apiBaseURL: string): boolean | undefined {
  const [edits, setEdits] = useState<boolean>()
  useEffect(() => {
    const abort = new AbortController()
    fetch(`${apiBaseURL}/config`, { signal: abort.signal })
      .then((response) => (response.ok ? response.json() : null))
      .then(
        (
          config: {
            capabilities?: { features?: Record<string, boolean> }
          } | null
        ) => setEdits(!!config?.capabilities?.features?.merchant_config_edits)
      )
      .catch(() => {
        if (!abort.signal.aborted) setEdits(false)
      })
    return () => abort.abort()
  }, [apiBaseURL])
  return edits
}

// NewMerchantPage creates a merchant the signed-in operator owns: the
// console's New merchant, and the first run's second step after the owner
// account. Only a root operator holding root:merchants:create may.
export function NewMerchantPage({
  auth,
  apiBaseURL,
  root,
  firstRun,
  onCreated,
}: {
  auth: AuthClient
  apiBaseURL: string
  root: boolean
  firstRun?: boolean
  onCreated(merchant: BillingAdminMerchant): void
}) {
  const navigate = useNavigate()
  const edits = useConfigurationEdits(apiBaseURL)
  const [name, setName] = useState("")
  const [displayName, setDisplayName] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  if (!root)
    return (
      <p className="console-empty">
        Only an operator creates merchants. <Link to="/">Open the console</Link>
      </p>
    )
  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      const response = await auth.authFetch(consoleURL("/merchants"), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name: name.trim(),
          display_name: edits ? displayName.trim() : "",
        }),
      })
      const body = (await response.json().catch(() => null)) as
        (BillingAdminMerchant & { error?: { message?: string } }) | null
      if (!response.ok || !body?.id) {
        setError(
          body?.error?.message ??
            `The merchant could not be created (${response.status}).`
        )
        return
      }
      onCreated(body)
      void navigate("/billing")
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : String(failure))
    } finally {
      setBusy(false)
    }
  }
  return (
    <section className="console-section console-form">
      {firstRun && (
        <ol className="console-steps" aria-label="Setup">
          <li data-done="true">Owner account</li>
          <li aria-current="step">First merchant</li>
          <li>Payment provider</li>
        </ol>
      )}
      <h1>{firstRun ? "Name your first merchant" : "New merchant"}</h1>
      <p className="console-description">
        A merchant is a business that takes payments here: its customers,
        catalog and payment providers. You own the merchant you create.
      </p>
      <form onSubmit={(event) => void submit(event)}>
        <label>
          Name
          <input
            name="name"
            required
            autoFocus
            autoComplete="off"
            maxLength={63}
            pattern="[a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?"
            value={name}
            onChange={(event) => setName(event.target.value.toLowerCase())}
            aria-describedby="merchant-name-hint"
          />
        </label>
        <small id="merchant-name-hint">
          Lowercase letters, digits and hyphens, such as acme-shop: how URLs and
          configuration name the merchant.
        </small>
        {edits && (
          <label>
            Display name (optional)
            <input
              name="display_name"
              autoComplete="organization"
              value={displayName}
              onChange={(event) => setDisplayName(event.target.value)}
            />
          </label>
        )}
        {error && <p role="alert">{error}</p>}
        <div className="console-actions">
          <button type="submit" disabled={busy || !name.trim()}>
            {busy ? "Creating…" : "Create merchant"}
          </button>
          {firstRun ? (
            <button type="button" onClick={() => void navigate("/")}>
              Skip for now
            </button>
          ) : (
            <Link to="/">Cancel</Link>
          )}
        </div>
      </form>
    </section>
  )
}
