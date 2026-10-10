/* eslint-disable react-refresh/only-export-components */
// The console's own navigation plus whatever its host's extensions add: one
// list the sidebar, layout and router read.
import * as React from "react"
import {
  CreditCardIcon,
  DashboardCircleIcon,
  PackageIcon,
  RepeatIcon,
  Settings01Icon,
  UserGroupIcon,
  Wrench01Icon,
} from "@hugeicons/core-free-icons"
import { matchPath, type RouteObject } from "react-router-dom"

import type {
  ConsoleContext,
  ConsoleExtension,
  ConsoleMenuItem,
  ConsoleMerchant,
  ConsoleNavItem,
  ConsoleRoute,
  ConsoleScope,
  ConsoleSettingsTab,
} from "./types"

export const BILLING_GROUP = "Billing"
export const ACCOUNT_GROUP = "Account"

export const coreNav: ConsoleNavItem[] = [
  { title: "Dashboard", path: "/", icon: DashboardCircleIcon, order: 0 },
  { title: "Customers", path: "/customers", icon: UserGroupIcon, order: 10 },
  {
    title: "Subscriptions",
    path: "/subscriptions",
    icon: RepeatIcon,
    order: 20,
  },
  {
    title: "Payments",
    path: "/payments",
    icon: CreditCardIcon,
    order: 30,
    items: [
      { title: "Payments", path: "/payments" },
      { title: "Health", path: "/payments/health" },
      { title: "Attempts", path: "/payments/attempts" },
      { title: "Rebill cycles", path: "/payments/cycles" },
    ],
  },
  { title: "Invoices", path: "/invoices", icon: CreditCardIcon, order: 40 },
  {
    title: "Catalog",
    path: "/catalog",
    icon: PackageIcon,
    order: 50,
    items: [
      { title: "Products", path: "/catalog" },
      { title: "Prices", path: "/catalog/prices" },
      { title: "Metering", path: "/catalog/metering" },
      { title: "Drift", path: "/catalog/drift" },
    ],
  },
  { title: "Ops", path: "/ops", icon: Wrench01Icon, order: 60 },
  { title: "Settings", path: "/settings", icon: Settings01Icon, order: 70 },
].map((item) => ({ ...item, scope: "merchant" as const, group: BILLING_GROUP }))

// Paths the console itself routes; an extension may not claim one.
export const corePaths = [
  "/login",
  "/",
  "/customers",
  "/customers/:customerId",
  "/subscriptions",
  "/subscriptions/:id",
  "/payments",
  "/payments/:id",
  "/payments/health",
  "/payments/attempts",
  "/payments/attempts/:id",
  "/payments/cycles",
  "/payments/cycles/:id",
  "/invoices",
  "/invoices/:id",
  "/catalog",
  "/catalog/prices",
  "/catalog/prices/:id",
  "/catalog/metering",
  "/catalog/metering/:key",
  "/catalog/drift",
  "/ops",
  "/settings",
]

// The Settings page's own tabs; an extension's tab may not reuse a value.
export const coreSettingsTabs = [
  "merchant",
  "notifications",
  "psps",
  "customer-controls",
]

const idPattern = /^[a-z0-9][a-z0-9-]*$/

function checkPath(where: string, path: string) {
  if (!path.startsWith("/") || path.startsWith("//") || /[\s?#]/.test(path)) {
    throw new Error(
      `console extension ${where}: path ${JSON.stringify(path)} must be a console path like "/account"`
    )
  }
}

// validateExtensions refuses a set that would collide with the console or with
// itself, so a bad host build fails at startup instead of hiding pages.
export function validateExtensions(extensions: ConsoleExtension[]) {
  const ids = new Set<string>()
  const paths = new Set(corePaths)
  const tabs = new Set(coreSettingsTabs)
  let newMerchant: string | undefined
  let directory: string | undefined
  for (const extension of extensions) {
    if (!idPattern.test(extension.id) || ids.has(extension.id)) {
      throw new Error(
        `console extension id ${JSON.stringify(extension.id)} must be unique lowercase letters, digits and dashes`
      )
    }
    ids.add(extension.id)
    for (const route of extension.routes ?? []) {
      checkPath(extension.id, route.path)
      if (paths.has(route.path)) {
        throw new Error(
          `console extension ${extension.id}: path ${route.path} is already routed`
        )
      }
      paths.add(route.path)
    }
    for (const item of [
      ...(extension.nav ?? []),
      ...(extension.userMenu ?? []),
    ]) {
      checkPath(extension.id, item.path)
    }
    if (extension.newMerchantPath !== undefined) {
      checkPath(extension.id, extension.newMerchantPath)
      if (newMerchant !== undefined) {
        throw new Error(
          `console extension ${extension.id}: only one extension may declare newMerchantPath`
        )
      }
      newMerchant = extension.newMerchantPath
    }
    if (extension.merchants !== undefined) {
      if (directory !== undefined) {
        throw new Error(
          `console extension ${extension.id}: only one extension may declare merchants (${directory} does)`
        )
      }
      directory = extension.id
    }
    for (const tab of extension.settingsTabs ?? []) {
      if (!idPattern.test(tab.value) || tabs.has(tab.value)) {
        throw new Error(
          `console extension ${extension.id}: settings tab ${JSON.stringify(tab.value)} must be a unique lowercase value`
        )
      }
      tabs.add(tab.value)
    }
  }
}

export interface NavGroup {
  label: string
  items: ConsoleNavItem[]
}

type ContextFor = (extensionId?: string) => ConsoleContext

// holds reports whether the merchant's known role is one of roles.
const holds = (merchant: ConsoleMerchant, roles: string[]) =>
  merchant.role !== undefined && roles.includes(merchant.role)

function shows(
  item: ConsoleNavItem | ConsoleMenuItem,
  ctx: ConsoleContext,
  scope: ConsoleScope
) {
  if (scope === "merchant") {
    if (!ctx.activeMerchant) return false
    const roles = "roles" in item ? item.roles : undefined
    if (roles && !holds(ctx.activeMerchant, roles)) return false
  }
  return item.visible ? item.visible(ctx) : true
}

// buildNav groups what this user may see: merchant groups (the console's
// "Billing" first) when a merchant is selected, then user groups, each sorted
// by order.
export function buildNav(
  extensions: ConsoleExtension[],
  contextFor: ContextFor
): NavGroup[] {
  const merchant = new Map<string, ConsoleNavItem[]>([[BILLING_GROUP, []]])
  const user = new Map<string, ConsoleNavItem[]>()
  const add = (item: ConsoleNavItem, ctx: ConsoleContext) => {
    if (!shows(item, ctx, item.scope)) return
    const groups = item.scope === "merchant" ? merchant : user
    const label =
      item.group ?? (item.scope === "merchant" ? BILLING_GROUP : ACCOUNT_GROUP)
    groups.set(label, [...(groups.get(label) ?? []), item])
  }
  const core = contextFor()
  coreNav.forEach((item) => add(item, core))
  for (const extension of extensions) {
    const ctx = contextFor(extension.id)
    extension.nav?.forEach((item) => add(item, ctx))
  }
  const byOrder = (a: ConsoleNavItem, b: ConsoleNavItem) =>
    (a.order ?? 100) - (b.order ?? 100) || a.title.localeCompare(b.title)
  return [...merchant, ...user]
    .filter(([, items]) => items.length > 0)
    .map(([label, items]) => ({ label, items: items.sort(byOrder) }))
}

// isWithin: "/" is only itself; any other path covers its sub-pages.
const isWithin = (pathname: string, path: string) =>
  path === "/"
    ? pathname === "/"
    : pathname === path || pathname.startsWith(`${path}/`)

// The entry for the page being looked at: the longest path that covers it.
export function activeNavPath(pathname: string, items: ConsoleNavItem[]) {
  return items
    .map((item) => item.path)
    .filter((path) => isWithin(pathname, path))
    .sort((a, b) => b.length - a.length)[0]
}

export function userMenuItems(
  extensions: ConsoleExtension[],
  contextFor: ContextFor
): ConsoleMenuItem[] {
  return extensions.flatMap((extension) =>
    (extension.userMenu ?? []).filter((item) =>
      shows(item, contextFor(extension.id), "user")
    )
  )
}

// directoryHost is the extension that lists the user's merchants, if one
// does.
export function directoryHost(
  extensions: ConsoleExtension[]
): ConsoleExtension | undefined {
  return extensions.find((extension) => extension.merchants)
}

// extensionSettingsTabs are the hosts' Settings tabs the selected merchant's
// role sees, by order.
export function extensionSettingsTabs(
  extensions: ConsoleExtension[],
  merchant?: ConsoleMerchant
): ConsoleSettingsTab[] {
  return extensions
    .flatMap((extension) => extension.settingsTabs ?? [])
    .filter((tab) => !tab.roles || (merchant && holds(merchant, tab.roles)))
    .sort((a, b) => (a.order ?? 100) - (b.order ?? 100))
}

export function newMerchantPath(extensions: ConsoleExtension[]) {
  return extensions.find((extension) => extension.newMerchantPath)
    ?.newMerchantPath
}

// extensionRoutes are the host's pages as children of the console shell.
export function extensionRoutes(
  extensions: ConsoleExtension[],
  loading: React.ReactNode
): RouteObject[] {
  return extensions.flatMap((extension) =>
    (extension.routes ?? []).map((route) => ({
      path: route.path.replace(/^\//, ""),
      hydrateFallbackElement: loading,
      lazy: route.lazy,
    }))
  )
}

// extensionRoute is the extension page at pathname, if one is.
export function extensionRoute(
  extensions: ConsoleExtension[],
  pathname: string
): ConsoleRoute | undefined {
  for (const extension of extensions) {
    for (const route of extension.routes ?? []) {
      if (matchPath(route.path, pathname)) return route
    }
  }
  return undefined
}

interface ExtensionsValue {
  extensions: ConsoleExtension[]
  config: Record<string, unknown>
}

const ExtensionsContext = React.createContext<ExtensionsValue>({
  extensions: [],
  config: {},
})

export function ConsoleExtensions({
  extensions,
  config,
  children,
}: ExtensionsValue & { children: React.ReactNode }) {
  const value = React.useMemo(
    () => ({ extensions, config }),
    [extensions, config]
  )
  let tree = children
  for (const extension of [...extensions].reverse()) {
    if (extension.Provider) {
      tree = <extension.Provider>{tree}</extension.Provider>
    }
  }
  return (
    <ExtensionsContext.Provider value={value}>
      {tree}
    </ExtensionsContext.Provider>
  )
}

export function useExtensions() {
  return React.useContext(ExtensionsContext)
}
