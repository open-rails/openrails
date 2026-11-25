/* eslint-disable react-refresh/only-export-components */
// AuthKit owns sign-in and refresh. Billing writes use their permissions directly.
import * as React from "react"
import { createAuthClient, type AuthClient } from "@openrails/auth-ui/client"
import { AuthUiProvider } from "@openrails/auth-ui/provider"
import { AuthProvider } from "@openrails/auth-ui/react"

import type { BootstrapConfig } from "../config"
import { LocalIdentity } from "@/lib/identity"
import { routerBasename } from "@/lib/mount"

// The refresh token lives in AuthKit's HttpOnly cookie; access tokens and a
// merchant's token stay in memory.

// consoleURL is path beneath the console's mount, absolute.
export function consoleURL(path: string): string {
  const base = routerBasename()
  return new URL(`${base === "/" ? "" : base}${path}`, window.location.origin)
    .href
}

// A merchant's token is exchanged through the console's own client, bound to
// this browser's key (a public client proves one), and kept in memory.
export function createConsoleSession(config: BootstrapConfig): AuthClient {
  const exchange = config.exchange
  return createAuthClient({
    baseUrl: config.auth_base_url,
    resourceTokens: exchange
      ? {
          clientId: exchange.client_id,
          tokenEndpoint: exchange.token_endpoint,
          dpop: true,
        }
      : undefined,
    sessionHint: false,
  })
}

export function ConsoleSession({
  auth,
  children,
}: {
  auth: AuthClient
  children: React.ReactNode
}) {
  return (
    <AuthProvider client={auth}>
      <LocalIdentity>
        <AuthUiProvider appearance={{ theme: "light" }}>
          {children}
        </AuthUiProvider>
      </LocalIdentity>
    </AuthProvider>
  )
}
