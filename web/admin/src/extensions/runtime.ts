// The bridge between the public extension API (public.ts) and the running
// console: main.tsx binds it at startup. Self-contained like types.ts.
import type { AuthClient } from "@openrails/auth-ui/client"

import type { ConsoleContext } from "./types"

export interface ConsoleHandle extends ConsoleContext {
  selectMerchant: (slug: string) => void
  logout: () => Promise<void>
}

export interface ConsoleRuntime {
  useConsole: (extensionId?: string) => ConsoleHandle
  // The deployment's own AuthKit session; undefined when the console signs
  // staff in at a trusted issuer.
  authClient: () => AuthClient | undefined
  // A fetch carrying whichever session the console holds.
  authFetch: (input: string | URL, init?: RequestInit) => Promise<Response>
  // The console's mount with a trailing slash, e.g. "/admin/".
  mountPath: () => string
}

let runtime: ConsoleRuntime | null = null

export function bindConsoleRuntime(next: ConsoleRuntime) {
  runtime = next
}

export function consoleRuntime(): ConsoleRuntime {
  if (!runtime) throw new Error("the merchant console has not started")
  return runtime
}
