// The console extension contract: what a host adds to the merchant console it
// builds (scripts/build-admin-console.sh --extensions). Self-contained on
// purpose — hosts type-check against these files without the console's
// internals.
import type { IconSvgElement } from "@hugeicons/react"
import type { ComponentType, ReactNode } from "react"

// "merchant" pages act on the selected merchant and need one; "user" pages
// belong to the signed-in person and stay reachable with no merchant at all.
export type ConsoleScope = "merchant" | "user"

export interface ConsoleMerchant {
  id: string
  slug: string
  display_name?: string
  // The member's role on this merchant: owner, support or viewer.
  role: string
}

export interface ConsoleUser {
  id: string
  username?: string
  email?: string
}

export interface ConsoleContext {
  me: ConsoleUser | null
  merchants: ConsoleMerchant[]
  activeMerchant?: ConsoleMerchant
  // The host's data for one extension: AdminConsoleConfig.Extensions[id].
  config: unknown
}

export interface ConsoleNavItem {
  title: string
  // A console path such as "/account", relative to the console's mount.
  path: string
  icon?: IconSvgElement
  scope: ConsoleScope
  // Sidebar group label. Merchant items default to the console's "Billing"
  // group; user items default to "Account".
  group?: string
  // Merchant roles that see a merchant-scoped item; unset is every role.
  roles?: string[]
  visible?: (ctx: ConsoleContext) => boolean
  // A React hook, for visibility that depends on data the extension loads
  // (the item renders once it returns true).
  useVisible?: () => boolean
  // Position within its group; the console's own items use 0–90 in tens.
  order?: number
  items?: { title: string; path: string }[]
}

export interface ConsoleRoute {
  // A console path, optionally with params: "/account", "/invoices/:id".
  path: string
  scope: ConsoleScope
  lazy: () => Promise<{ Component: ComponentType }>
  trail?: { label: string; to?: string }[]
}

export interface ConsoleMenuItem {
  title: string
  path: string
  icon?: IconSvgElement
  visible?: (ctx: ConsoleContext) => boolean
  useVisible?: () => boolean
}

export interface ConsoleExtension {
  // Unique; also the key of this extension's AdminConsoleConfig.Extensions
  // entry.
  id: string
  nav?: ConsoleNavItem[]
  routes?: ConsoleRoute[]
  // Account-menu entries, above the console's own.
  userMenu?: ConsoleMenuItem[]
  // A console path that creates a merchant. The empty state and the merchant
  // switcher offer "New merchant" when one extension declares it.
  newMerchantPath?: string
  // Wraps the whole console, inside its session and query providers: host
  // context, dialogs a host page relies on.
  Provider?: ComponentType<{ children: ReactNode }>
}
