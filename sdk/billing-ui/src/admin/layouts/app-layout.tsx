import { useQuery } from "@tanstack/react-query"
import { useAdminContext } from "../context"
import { AdminMessage } from "../ui-context"
import type { ReactNode } from "react"
import { useLocation } from "#orck/admin/router"

import { AppSidebar } from "#orck/admin/components/app-sidebar"
import { NoAccess } from "#orck/admin/components/no-access"
import { NoMerchants } from "#orck/admin/components/no-merchants"
import { SiteHeader } from "#orck/admin/components/site-header"
import {
  SidebarInset,
  SidebarProvider,
} from "#orck/admin/components/ui/sidebar"
import { TooltipProvider } from "#orck/admin/components/ui/tooltip"
import type { AdminAccess } from "#orck/client/generated/wire"
import { useAuth } from "#orck/admin/lib/auth"
import { hasAnyArea } from "#orck/admin/lib/capabilities"

// Longest path first: the catalog sub-pages have to match before the section
// they live under. A trail of more than one entry becomes a real breadcrumb.
const trails: [string, { label: string; to?: string }[]][] = [
  ["/customers", [{ label: "Customers" }]],
  ["/subscriptions", [{ label: "Subscriptions" }]],
  ["/payments", [{ label: "Payments" }]],
  [
    "/catalog/metering",
    [{ label: "Catalog", to: "/catalog" }, { label: "Metering" }],
  ],
  [
    "/catalog/prices",
    [{ label: "Catalog", to: "/catalog" }, { label: "Prices" }],
  ],
  [
    "/catalog/drift",
    [{ label: "Catalog", to: "/catalog" }, { label: "Drift" }],
  ],
  ["/catalog", [{ label: "Catalog", to: "/catalog" }, { label: "Products" }]],
  [
    "/ops/imports",
    [
      { label: "Ops", to: "/ops" },
      { label: "PSP imports", to: "/ops/imports" },
    ],
  ],
  [
    "/ops/provider-prices",
    [{ label: "Ops", to: "/ops" }, { label: "Provider prices" }],
  ],
  ["/ops/parked", [{ label: "Ops", to: "/ops" }, { label: "Parked" }]],
  ["/ops", [{ label: "Ops", to: "/ops" }, { label: "Findings" }]],
  ["/settings", [{ label: "Settings" }]],
]

// areaOf is the console area a merchant page belongs to, and whether the
// caller may use it; Settings belongs to none (its tabs check their own).
function areaOf(
  pathname: string,
  access: AdminAccess
): { name: string; allowed: boolean } | null {
  const under = (prefix: string) =>
    pathname === prefix || pathname.startsWith(prefix + "/")
  if (under("/payments/health"))
    return { name: "payment health", allowed: access.metrics === true }
  if (["/customers", "/subscriptions", "/payments", "/orders"].some(under))
    return {
      name: "customer support",
      allowed: access.admin === "read" || access.admin === "update",
    }
  if (under("/ops") || under("/catalog/drift"))
    return {
      name: "merchant configuration",
      allowed:
        access.merchant_config === "read" ||
        access.merchant_config === "update",
    }
  if (under("/catalog"))
    return {
      name: "the catalog",
      allowed: access.catalog === "read" || access.catalog === "update",
    }
  return null
}

export function AppLayout({ children }: { children: ReactNode }) {
  const { activeMerchant } = useAuth()
  const { pathname } = useLocation()
  const { props, services } = useAdminContext()
  const permission = useQuery(services.adminQueries.access())
  const access = permission.data
  const hostSettings =
    (props.settingsTabs?.length ?? 0) > 0 &&
    (pathname === "/settings" || pathname === "/")
  const trail =
    trails.find(([prefix]) => pathname.startsWith(prefix))?.[1] ?? []
  if (!activeMerchant) return <NoMerchants />
  if (permission.isPending)
    return (
      <p role="status" className="p-6">
        <AdminMessage id="Loading billing permissions…" />
      </p>
    )
  if (permission.error)
    return (
      <div role="alert" className="p-6">
        <p>{permission.error.message}</p>
        <button type="button" onClick={() => void permission.refetch()}>
          <AdminMessage id="Try again" />
        </button>
      </div>
    )
  if (
    activeMerchant.id &&
    access?.scope_id &&
    activeMerchant.id !== access.scope_id
  )
    return (
      <p role="alert" className="p-6">
        <AdminMessage id="The authenticated connection is bound to another merchant. Update the host connection before opening this merchant." />
      </p>
    )
  return (
    <TooltipProvider>
      <SidebarProvider>
        <AppSidebar />
        <SidebarInset>
          <SiteHeader trail={trail} />
          {/* A switch remounts the merchant's pages: nothing of the
              previous merchant's survives it. */}
          <main
            key={activeMerchant?.id || activeMerchant?.slug || ""}
            className="flex min-w-0 flex-1 flex-col p-4 md:p-8"
          >
            <div className="mx-auto w-full max-w-7xl min-w-0">
              {access &&
              ((!hasAnyArea(access) && !hostSettings) ||
                areaOf(pathname, access)?.allowed === false) ? (
                <NoAccess area={areaOf(pathname, access)?.name} />
              ) : (
                children
              )}
            </div>
          </main>
        </SidebarInset>
      </SidebarProvider>
    </TooltipProvider>
  )
}
