import { Navigate, Outlet, useLocation } from "react-router-dom"

import { AppSidebar } from "@/components/app-sidebar"
import { SiteHeader } from "@/components/site-header"
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar"
import { TooltipProvider } from "@/components/ui/tooltip"
import { useAuth } from "@/lib/auth"

const titles: [string, string][] = [
  ["/customers", "Customers"],
  ["/subscriptions", "Subscriptions"],
  ["/payments", "Payments"],
  ["/catalog", "Catalog"],
  ["/ops", "Ops"],
  ["/settings", "Settings"],
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

  const title =
    titles.find(([prefix]) => pathname.startsWith(prefix))?.[1] ?? "Dashboard"

  return (
    <TooltipProvider>
      <SidebarProvider>
        <AppSidebar />
        <SidebarInset>
          <SiteHeader title={title} />
          <main className="flex flex-1 flex-col p-4 md:p-8">
            <div className="mx-auto w-full max-w-5xl">
              <Outlet />
            </div>
          </main>
        </SidebarInset>
      </SidebarProvider>
    </TooltipProvider>
  )
}
