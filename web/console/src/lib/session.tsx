/* eslint-disable react-refresh/only-export-components */
// AuthKit owns sign-in and refresh. Billing writes use their permissions directly.
import * as React from "react"
import {
  createAuthClient,
  createIssuerClient,
  type AuthClient,
  type IssuerClient,
} from "@openrails/auth-ui/client"
import { AuthUiProvider } from "@openrails/auth-ui/provider"
import { AuthProvider, IssuerAuthProvider } from "@openrails/auth-ui/react"

import type { BootstrapConfig, ConsoleIssuer } from "../config"
import { IssuerIdentity, LocalIdentity } from "@/lib/identity"
import { routerBasename } from "@/lib/mount"

// The refresh token lives in sessionStorage: it stays with the tab. A
// merchant's token never leaves memory.
const REFRESH_KEY = "openrails.admin.refresh"

export type ConsoleClient =
  | { kind: "local"; client: AuthClient }
  | { kind: "issuer"; client: IssuerClient; issuer: ConsoleIssuer }

// consoleURL is path beneath the console's mount, absolute.
export function consoleURL(path: string): string {
  const base = routerBasename()
  return new URL(`${base === "/" ? "" : base}${path}`, window.location.origin)
    .href
}

export function createConsoleSession(
  config: BootstrapConfig,
  issuer?: ConsoleIssuer
): ConsoleClient {
  if (issuer) {
    const client = createIssuerClient({
      issuer: issuer.url,
      clientId: issuer.client_id,
      redirectUri: consoleURL("/callback"),
      resource: issuer.resource,
      scope: issuer.scope,
      postLogoutRedirectUri: consoleURL("/login"),
    })
    return { kind: "issuer", client, issuer }
  }
  // A merchant's token is exchanged through the console's own client, bound
  // to this browser's key (a public client proves one), and kept in memory.
  const exchange = config.exchange
  const client = createAuthClient({
    baseUrl: config.auth_base_url,
    resourceTokens: exchange
      ? {
          clientId: exchange.client_id,
          tokenEndpoint: exchange.token_endpoint,
          dpop: true,
        }
      : undefined,
    storage: {
      get: () => sessionStorage.getItem(REFRESH_KEY),
      set: (token) =>
        token
          ? sessionStorage.setItem(REFRESH_KEY, token)
          : sessionStorage.removeItem(REFRESH_KEY),
    },
    sessionHint: false,
  })
  return { kind: "local", client }
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
        <IssuerIdentity name={session.issuer.name}>{children}</IssuerIdentity>
      </IssuerAuthProvider>
    )
  }
  return (
    <AuthProvider client={session.client}>
      <LocalIdentity>
        <AuthUiProvider appearance={{ theme: "light" }}>
          {children}
        </AuthUiProvider>
      </LocalIdentity>
    </AuthProvider>
  )
}
