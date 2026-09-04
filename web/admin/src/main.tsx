import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { createBrowserRouter, RouterProvider } from "react-router-dom"
import { QueryClientProvider } from "@tanstack/react-query"

import "./index.css"
import { ThemeProvider } from "@/components/theme-provider"
import { Toaster } from "@/components/ui/sonner"
import { AppLayout } from "@/layouts/app-layout"
import { AuthProvider } from "@/lib/auth"
import { queryClient } from "@/lib/query-client"
<<<<<<< HEAD
const routeLoading = (
  <div role="status" className="p-6 text-sm text-muted-foreground">
    Loading…
  </div>
)
=======
import {
  CatalogDriftPage,
  CatalogPricesPage,
  CatalogProductsPage,
} from "@/pages/catalog"
import { PriceDetailPage } from "@/pages/catalog/price-detail"
import { CatalogMeteringPage, MeterDetailPage } from "@/pages/catalog/metering"
import { CustomersPage } from "@/pages/customers"
import { CustomerDetailPage } from "@/pages/customers/detail"
import { DashboardPage } from "@/pages/dashboard"
import { LoginPage } from "@/pages/login"
import { OpsPage } from "@/pages/ops"
import { InvoicesPage } from "@/pages/invoices"
import { InvoiceDetailPage } from "@/pages/invoices/detail"
import { PaymentsPage } from "@/pages/payments"
import { PaymentDetailPage } from "@/pages/payments/detail"
import { SettingsPage } from "@/pages/settings"
import { SubscriptionsPage } from "@/pages/subscriptions"
import { SubscriptionDetailPage } from "@/pages/subscriptions/detail"
>>>>>>> origin/master

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
      path: "/",
      hydrateFallbackElement: routeLoading,
      element: <AppLayout />,
      children: [
<<<<<<< HEAD
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
=======
        { index: true, element: <DashboardPage /> },
        { path: "customers", element: <CustomersPage /> },
        { path: "customers/:customerId", element: <CustomerDetailPage /> },
        { path: "subscriptions", element: <SubscriptionsPage /> },
        { path: "subscriptions/:id", element: <SubscriptionDetailPage /> },
        { path: "payments", element: <PaymentsPage /> },
        { path: "invoices", element: <InvoicesPage /> },
        { path: "invoices/:id", element: <InvoiceDetailPage /> },
        { path: "payments/:id", element: <PaymentDetailPage /> },
        { path: "catalog", element: <CatalogProductsPage /> },
        { path: "catalog/prices", element: <CatalogPricesPage /> },
        { path: "catalog/prices/:id", element: <PriceDetailPage /> },
        { path: "catalog/metering", element: <CatalogMeteringPage /> },
        { path: "catalog/metering/:key", element: <MeterDetailPage /> },
        { path: "catalog/drift", element: <CatalogDriftPage /> },
        { path: "ops", element: <OpsPage /> },
        { path: "settings", element: <SettingsPage /> },
>>>>>>> origin/master
      ],
    },
  ],
  { basename: "/admin" }
)

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ThemeProvider>
      <QueryClientProvider client={queryClient}>
        <AuthProvider>
          <RouterProvider router={router} />
          <Toaster />
        </AuthProvider>
      </QueryClientProvider>
    </ThemeProvider>
  </StrictMode>
)
