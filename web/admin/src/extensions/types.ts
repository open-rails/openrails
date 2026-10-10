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
  // Empty for a merchant opened by name, before the host's directory knows it.
  id: string
  slug: string
  display_name?: string
  // The member's role on this merchant (owner, support or viewer), when a
  // host's directory says it.
  role?: string
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

// A tab of the console's Settings page: a host's team, API keys or the like.
export interface ConsoleSettingsTab {
  // The ?tab= value; unique, and not one of the console's own.
  value: string
  title: string
  lazy: () => Promise<{ Component: ComponentType }>
  // Position among the tabs; the console's own use 0–50 in tens.
  order?: number
  // Merchant roles that see the tab; unset is every role.
  roles?: string[]
}

export interface ConsoleExtension {
  // Unique; also the key of this extension's AdminConsoleConfig.Extensions
  // entry.
  id: string
  nav?: ConsoleNavItem[]
  routes?: ConsoleRoute[]
  // Account-menu entries, above the console's own.
  userMenu?: ConsoleMenuItem[]
  // The signed-in user's merchants, from the host's own directory: the
  // switcher's list. Without one the console acts for the merchant its mount
  // serves, or one the user opens by name. At most one extension declares it.
  merchants?: () => Promise<ConsoleMerchant[]>
  // A console path that creates a merchant. The empty state and the merchant
  // switcher offer "New merchant" when one extension declares it.
  newMerchantPath?: string
  // Rendered in the no-merchant state below the console's own text, such as
  // the invitations a user may accept.
  EmptyState?: ComponentType
  // Tabs added to the Settings page of the selected merchant.
  settingsTabs?: ConsoleSettingsTab[]
  // Wraps the whole console, inside its session and query providers: host
  // context, dialogs a host page relies on.
  Provider?: ComponentType<{ children: ReactNode }>
}
