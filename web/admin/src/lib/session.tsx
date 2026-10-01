/* eslint-disable react-refresh/only-export-components */
// The console's AuthKit session is auth-ui's: its client holds the bearer and
// refreshes it, and its step-up dialog (lib/step-up-host) answers OpenRails'
// step_up_required on every write (lib/api/client api()).
import * as React from "react"
import { createAuthClient, type AuthClient } from "@openrails/auth-ui/client"
import { AuthUiProvider } from "@openrails/auth-ui/provider"
import { AuthProvider } from "@openrails/auth-ui/react"

import {
  bindSession,
  setSelectedMerchant,
  type BootstrapConfig,
} from "@/lib/api/client"
import { queryClient } from "@/lib/query-client"

const StepUpHost = React.lazy(() => import("@/lib/step-up-host"))

// The refresh token stays with the tab, as the session always has.
const REFRESH_KEY = "openrails.admin.refresh"

export function createConsoleSession(config: BootstrapConfig): AuthClient {
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
  return client
}

export function ConsoleSession({
  client,
  children,
}: {
  client: AuthClient
  children: React.ReactNode
}) {
  return (
    <AuthProvider
      client={client}
      // Another user's merchant data never outlives a sign-out or switch.
      onUserChange={(_, previous) => {
        if (previous === null) return
        setSelectedMerchant(undefined)
        queryClient.removeQueries()
      }}
    >
      <AuthUiProvider appearance={{ theme: "inherit" }}>
        {children}
        <React.Suspense fallback={null}>
          <StepUpHost />
        </React.Suspense>
      </AuthUiProvider>
    </AuthProvider>
  )
}
