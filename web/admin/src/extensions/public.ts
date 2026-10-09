// @openrails/console: the API a host's console extensions import. It resolves
// to this file in a console build with extensions.
import type { AuthClient } from "@openrails/auth-ui/client"

import { consoleRuntime, type ConsoleHandle } from "./runtime"
import type { ConsoleExtension } from "./types"

export type * from "./types"
export type { ConsoleHandle } from "./runtime"

// defineConsoleExtension types an extension declaration.
export function defineConsoleExtension(extension: ConsoleExtension) {
  return extension
}

// useConsole is the signed-in user, their merchants and the selected one; with
// an extension id, config is that extension's AdminConsoleConfig.Extensions
// entry.
export function useConsole(extensionId?: string): ConsoleHandle {
  return consoleRuntime().useConsole(extensionId)
}

// authClient is the console's AuthKit session when it signs in to the
// deployment's own accounts (undefined with a trusted issuer). Host pages use
// it rather than starting a second session.
export function authClient(): AuthClient | undefined {
  return consoleRuntime().authClient()
}

// authFetch calls the host's own APIs with the console's session, local or
// a trusted issuer's.
export function authFetch(
  input: string | URL,
  init?: RequestInit
): Promise<Response> {
  return consoleRuntime().authFetch(input, init)
}

// consoleHref is the URL of a console path; with a merchant, the console opens
// on it (#merchant=<slug>) when the user belongs to it.
export function consoleHref(path: string, merchant?: string): string {
  const href = consoleRuntime().mountPath() + path.replace(/^\/+/, "")
  return merchant ? `${href}#merchant=${encodeURIComponent(merchant)}` : href
}
