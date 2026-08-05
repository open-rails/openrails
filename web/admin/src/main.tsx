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
import { CatalogPage } from "@/pages/catalog"
import { PriceDetailPage } from "@/pages/catalog/price-detail"
import { CustomersPage } from "@/pages/customers"
import { CustomerDetailPage } from "@/pages/customers/detail"
import { DashboardPage } from "@/pages/dashboard"
import { LoginPage } from "@/pages/login"
import { OpsPage } from "@/pages/ops"
import { PaymentsPage } from "@/pages/payments"
import { PaymentDetailPage } from "@/pages/payments/detail"
import { SettingsPage } from "@/pages/settings"
import { SubscriptionsPage } from "@/pages/subscriptions"
import { SubscriptionDetailPage } from "@/pages/subscriptions/detail"

const router = createBrowserRouter(
  [
    { path: "/login", element: <LoginPage /> },
    {
      path: "/",
      element: <AppLayout />,
      children: [
        { index: true, element: <DashboardPage /> },
        { path: "customers", element: <CustomersPage /> },
        { path: "customers/:customerId", element: <CustomerDetailPage /> },
        { path: "subscriptions", element: <SubscriptionsPage /> },
        { path: "subscriptions/:id", element: <SubscriptionDetailPage /> },
        { path: "payments", element: <PaymentsPage /> },
        { path: "payments/:id", element: <PaymentDetailPage /> },
        { path: "catalog", element: <CatalogPage /> },
        { path: "catalog/prices/:id", element: <PriceDetailPage /> },
        { path: "ops", element: <OpsPage /> },
        { path: "settings", element: <SettingsPage /> },
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
