import { Navigate, Outlet, useLocation } from "react-router-dom"

import { AppSidebar } from "@/components/app-sidebar"
import { NoAccess } from "@/components/no-access"
import { NoMerchants } from "@/components/no-merchants"
import { SiteHeader } from "@/components/site-header"
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar"
import { TooltipProvider } from "@/components/ui/tooltip"
import { extensionRoute, useExtensions } from "@/extensions/registry"
import type { AdminAccess } from "@/lib/api/generated/wire"
import { useAuth } from "@/lib/auth"
import { hasAnyArea, useAccess } from "@/lib/capabilities"

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
  ["/ops", [{ label: "Ops" }]],
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
    return { name: "payment health", allowed: access.metrics }
  if (
    ["/customers", "/subscriptions", "/payments", "/invoices", "/ops"].some(
      under
    )
  )
    return { name: "customer support", allowed: access.admin !== "none" }
  if (under("/catalog")) return { name: "the catalog", allowed: access.catalog }
  return null
}

export function AppLayout() {
  const { ready, signedIn, merchants, merchantsFailed } = useAuth()
  const { pathname } = useLocation()
  const access = useAccess()
  const { extensions } = useExtensions()
  const route = extensionRoute(extensions, pathname)
  const scope = route?.scope ?? "merchant"
  // A host with pages of its own keeps its shell for a user with no merchant.
  const hasUserPages = extensions.some((extension) =>
    extension.routes?.some((route) => route.scope === "user")
  )

  if (!ready) {
    return (
      <div className="flex min-h-svh items-center justify-center text-sm text-muted-foreground">
        Loading…
      </div>
    )
  }
  if (!signedIn) return <Navigate to="/login" replace />
  if (merchantsFailed) {
    return (
      <div className="flex min-h-svh items-center justify-center p-6 text-center text-sm text-muted-foreground">
        <div>
          <p>Could not load your merchants.</p>
          <button
            type="button"
            className="mt-2 text-primary underline"
            onClick={() => window.location.reload()}
          >
            Try again
          </button>
        </div>
      </div>
    )
  }
  const noMerchants = merchants.length === 0
  if (noMerchants && !hasUserPages) return <NoMerchants />

  const trail =
    route?.trail ??
    trails.find(([prefix]) => pathname.startsWith(prefix))?.[1] ??
    []

  return (
    <TooltipProvider>
      <SidebarProvider>
        <AppSidebar />
        <SidebarInset>
          <SiteHeader trail={trail} />
          <main className="flex min-w-0 flex-1 flex-col p-4 md:p-8">
            <div className="mx-auto w-full max-w-7xl min-w-0">
              {noMerchants && scope === "merchant" ? (
                <NoMerchants inShell />
              ) : scope === "merchant" && access && !hasAnyArea(access) ? (
                <NoAccess />
              ) : scope === "merchant" &&
                access &&
                areaOf(pathname, access)?.allowed === false ? (
                <NoAccess area={areaOf(pathname, access)?.name} />
              ) : (
                <Outlet />
              )}
            </div>
          </main>
        </SidebarInset>
      </SidebarProvider>
    </TooltipProvider>
  )
}
