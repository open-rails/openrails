import { Navigate, Outlet, useLocation } from "react-router-dom"

import { AppSidebar } from "@/components/app-sidebar"
import { SiteHeader } from "@/components/site-header"
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar"
import { TooltipProvider } from "@/components/ui/tooltip"
import { useAuth } from "@/lib/auth"

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

export function AppLayout() {
  const { ready, bootError, me } = useAuth()
  const { pathname } = useLocation()

  if (!ready) {
    return (
      <div className="flex min-h-svh items-center justify-center text-sm text-muted-foreground">
        Loading…
      </div>
    )
  }
  if (bootError) {
    return (
      <div className="flex min-h-svh items-center justify-center p-6 text-center">
        <div>
          <p className="font-medium">Admin console failed to start</p>
          <p className="mt-1 text-sm text-muted-foreground">{bootError}</p>
        </div>
      </div>
    )
  }
  if (!me) {
    return <Navigate to="/login" replace />
  }

  const trail =
    trails.find(([prefix]) => pathname.startsWith(prefix))?.[1] ?? []

  return (
    <TooltipProvider>
      <SidebarProvider>
        <AppSidebar />
        <SidebarInset>
          <SiteHeader trail={trail} />
          <main className="flex min-w-0 flex-1 flex-col p-4 md:p-8">
            <div className="mx-auto w-full max-w-7xl min-w-0">
              <Outlet />
            </div>
          </main>
        </SidebarInset>
      </SidebarProvider>
    </TooltipProvider>
  )
}
