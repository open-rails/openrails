/* eslint-disable react-refresh/only-export-components */
// The console's session is auth-ui's: its AuthKit client against the
// deployment's own accounts, or its issuer client against a trusted issuer.
// Either holds the bearer and refreshes it; a write OpenRails refuses with
// step_up_required re-authenticates (lib/api/client api()).
import * as React from "react"
import {
  createAuthClient,
  createIssuerClient,
  isAuthKitError,
  type AuthClient,
  type IssuerClient,
} from "@openrails/auth-ui/client"
import { AuthUiProvider } from "@openrails/auth-ui/provider"
import {
  AuthProvider,
  IssuerAuthProvider,
  type Guard,
} from "@openrails/auth-ui/react"

import {
  bindSession,
  bindStepUp,
  setSelectedMerchant,
  type BootstrapConfig,
} from "@/lib/api/client"
import { IssuerIdentity, LocalIdentity } from "@/lib/identity"
import { routerBasename } from "@/lib/mount"
import { queryClient } from "@/lib/query-client"

const StepUpHost = React.lazy(() => import("@/lib/step-up-host"))

// The refresh token stays with the tab, as the session always has.
const REFRESH_KEY = "openrails.admin.refresh"

export type ConsoleClient =
  | { kind: "local"; client: AuthClient }
  | { kind: "issuer"; client: IssuerClient; name: string }

// consoleURL is path beneath the console's mount, absolute.
export function consoleURL(path: string): string {
  const base = routerBasename()
  return new URL(`${base === "/" ? "" : base}${path}`, window.location.origin)
    .href
}

export function createConsoleSession(config: BootstrapConfig): ConsoleClient {
  if (config.issuer) {
    const client = createIssuerClient({
      issuer: config.issuer.url,
      clientId: config.issuer.client_id,
      redirectUri: consoleURL("/callback"),
      resource: config.issuer.resource,
      scope: config.issuer.scope,
      postLogoutRedirectUri: consoleURL("/login"),
    })
    bindSession(client)
    return { kind: "issuer", client, name: config.issuer.name }
  }
  const client = createAuthClient({
    baseUrl: config.auth_base_url,
    storage: {
      get: () => sessionStorage.getItem(REFRESH_KEY),
      set: (token) =>
        token
          ? sessionStorage.setItem(REFRESH_KEY, token)
          : sessionStorage.removeItem(REFRESH_KEY),
    },
    sessionHint: false,
  })
  bindSession(client)
  return { kind: "local", client }
}

// Another user's merchant data never outlives a sign-out or switch.
function forgetPreviousUser(previous: string | null) {
  if (previous === null) return
  setSelectedMerchant(undefined)
  queryClient.removeQueries()
}

export function ConsoleSession({
  session,
  children,
}: {
  session: ConsoleClient
  children: React.ReactNode
}) {
  if (session.kind === "issuer") {
    return (
      <IssuerAuthProvider client={session.client}>
        <IssuerIdentity name={session.name} onUserChange={forgetPreviousUser}>
          <IssuerStepUp client={session.client} />
          {children}
        </IssuerIdentity>
      </IssuerAuthProvider>
    )
  }
  return (
    <AuthProvider
      client={session.client}
      onUserChange={(_, previous) => forgetPreviousUser(previous)}
    >
      <LocalIdentity>
        <AuthUiProvider appearance={{ theme: "inherit" }}>
          {children}
          <React.Suspense fallback={null}>
            <StepUpHost />
          </React.Suspense>
        </AuthUiProvider>
      </LocalIdentity>
    </AuthProvider>
  )
}

// issuerStepUp answers step_up_required with a fresh sign-in at the issuer
// (max_age=0) in a popup, then runs the write again.
export function issuerStepUp(client: Pick<IssuerClient, "stepUp">): Guard {
  return async (action) => {
    try {
      return await action()
    } catch (error) {
      if (!isAuthKitError(error) || error.code !== "step_up_required")
        throw error
      await client.stepUp({ popup: true })
      return action()
    }
  }
}

function IssuerStepUp({ client }: { client: IssuerClient }) {
  React.useEffect(() => {
    bindStepUp(issuerStepUp(client))
    return () => bindStepUp(null)
  }, [client])
  return null
}
