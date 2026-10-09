import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { createBrowserRouter, RouterProvider } from "react-router-dom"
import { QueryClientProvider } from "@tanstack/react-query"

import "./index.css"
import { ThemeProvider } from "@/components/theme-provider"
import { Toaster } from "@/components/ui/sonner"
import extensions from "@/extensions/host"
import {
  ConsoleExtensions,
  extensionRoutes,
  validateExtensions,
} from "@/extensions/registry"
import { bindConsoleRuntime } from "@/extensions/runtime"
import { useConsoleHandle } from "@/extensions/use-console"
import { AppLayout } from "@/layouts/app-layout"
import { loadBootstrap } from "@/lib/api/client"
import { bootstrapURL, routerBasename } from "@/lib/mount"
import { queryClient } from "@/lib/query-client"
import { ConsoleSession, createConsoleSession } from "@/lib/session"
const routeLoading = (
  <div role="status" className="p-6 text-sm text-muted-foreground">
    Loading…
  </div>
)

validateExtensions(extensions)

const router = createBrowserRouter(
  [
    {
      path: "/login",
      hydrateFallbackElement: routeLoading,
      lazy: () =>
        import("@/pages/login").then((module) => ({
          Component: module.LoginPage,
        })),
    },
    {
      path: "/callback",
      hydrateFallbackElement: routeLoading,
      lazy: () =>
        import("@/pages/callback").then((module) => ({
          Component: module.CallbackPage,
        })),
    },
    {
      path: "/",
      hydrateFallbackElement: routeLoading,
      element: <AppLayout />,
      children: [
        {
          index: true,
          lazy: () =>
            import("@/pages/dashboard").then((module) => ({
              Component: module.DashboardPage,
            })),
        },
        {
          path: "customers",
          lazy: () =>
            import("@/pages/customers").then((module) => ({
              Component: module.CustomersPage,
            })),
        },
        {
          path: "customers/:customerId",
          lazy: () =>
            import("@/pages/customers/detail").then((module) => ({
              Component: module.CustomerDetailPage,
            })),
        },
        {
          path: "subscriptions",
          lazy: () =>
            import("@/pages/subscriptions").then((module) => ({
              Component: module.SubscriptionsPage,
            })),
        },
        {
          path: "subscriptions/:id",
          lazy: () =>
            import("@/pages/subscriptions/detail").then((module) => ({
              Component: module.SubscriptionDetailPage,
            })),
        },
        {
          path: "payments",
          lazy: () =>
            import("@/pages/payments").then((module) => ({
              Component: module.PaymentsPage,
            })),
        },
        {
          path: "payments/:id",
          lazy: () =>
            import("@/pages/payments/detail").then((module) => ({
              Component: module.PaymentDetailPage,
            })),
        },
        {
          path: "payments/health",
          lazy: () =>
            import("@/pages/payments/health").then((module) => ({
              Component: module.PaymentHealthPage,
            })),
        },
        {
          path: "payments/attempts",
          lazy: () =>
            import("@/pages/payments/attempts").then((module) => ({
              Component: module.AttemptsPage,
            })),
        },
        {
          path: "payments/attempts/:id",
          lazy: () =>
            import("@/pages/payments/attempts").then((module) => ({
              Component: module.AttemptDetailPage,
            })),
        },
        {
          path: "payments/cycles",
          lazy: () =>
            import("@/pages/payments/attempts").then((module) => ({
              Component: module.CyclesPage,
            })),
        },
        {
          path: "payments/cycles/:id",
          lazy: () =>
            import("@/pages/payments/attempts").then((module) => ({
              Component: module.CycleDetailPage,
            })),
        },
        {
          path: "invoices",
          lazy: () =>
            import("@/pages/invoices").then((module) => ({
              Component: module.InvoicesPage,
            })),
        },
        {
          path: "invoices/:id",
          lazy: () =>
            import("@/pages/invoices/detail").then((module) => ({
              Component: module.InvoiceDetailPage,
            })),
        },
        {
          path: "catalog",
          lazy: () =>
            import("@/pages/catalog").then((module) => ({
              Component: module.CatalogProductsPage,
            })),
        },
        {
          path: "catalog/prices",
          lazy: () =>
            import("@/pages/catalog").then((module) => ({
              Component: module.CatalogPricesPage,
            })),
        },
        {
          path: "catalog/prices/:id",
          lazy: () =>
            import("@/pages/catalog/price-detail").then((module) => ({
              Component: module.PriceDetailPage,
            })),
        },
        {
          path: "catalog/metering",
          lazy: () =>
            import("@/pages/catalog/metering").then((module) => ({
              Component: module.CatalogMeteringPage,
            })),
        },
        {
          path: "catalog/metering/:key",
          lazy: () =>
            import("@/pages/catalog/metering").then((module) => ({
              Component: module.MeterDetailPage,
            })),
        },
        {
          path: "catalog/drift",
          lazy: () =>
            import("@/pages/catalog").then((module) => ({
              Component: module.CatalogDriftPage,
            })),
        },
        {
          path: "ops",
          lazy: () =>
            import("@/pages/ops").then((module) => ({
              Component: module.OpsPage,
            })),
        },
        {
          path: "settings",
          lazy: () =>
            import("@/pages/settings").then((module) => ({
              Component: module.SettingsPage,
            })),
        },
        ...extensionRoutes(extensions, routeLoading),
      ],
    },
  ],
  { basename: routerBasename() }
)

const root = createRoot(document.getElementById("root")!)
loadBootstrap(bootstrapURL()).then(
  (config) => {
    const session = createConsoleSession(config)
    bindConsoleRuntime({
      useConsole: useConsoleHandle,
      authClient: () => (session.kind === "local" ? session.client : undefined),
      authFetch: (input, init) => session.client.authFetch(input, init),
      mountPath: () => `${routerBasename().replace(/\/$/, "")}/`,
    })
    root.render(
      <StrictMode>
        <ThemeProvider>
          <QueryClientProvider client={queryClient}>
            <ConsoleSession session={session}>
              <ConsoleExtensions
                extensions={extensions}
                config={config.extensions ?? {}}
              >
                <RouterProvider router={router} />
              </ConsoleExtensions>
              <Toaster />
            </ConsoleSession>
          </QueryClientProvider>
        </ThemeProvider>
      </StrictMode>
    )
  },
  (error: unknown) =>
    root.render(
      <div className="flex min-h-svh items-center justify-center p-6 text-center">
        <div>
          <p className="font-medium">Admin console failed to start</p>
          <p className="mt-1 text-sm text-muted-foreground">
            {error instanceof Error ? error.message : String(error)}
          </p>
        </div>
      </div>
    )
)
