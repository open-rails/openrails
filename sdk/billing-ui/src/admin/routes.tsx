import { AdminMessage } from "#orck/admin/ui-context"
import { lazy, Suspense } from "react"
import { RouteParams, useLocation } from "./router"

const routes = [
  {
    path: "/payments/attempts",
    Component: lazy(() =>
      import("./pages/payments/attempts").then((m) => ({
        default: m.AttemptsPage,
      }))
    ),
  },
  {
    path: "/catalog/metering",
    Component: lazy(() =>
      import("./pages/catalog/metering").then((m) => ({
        default: m.CatalogMeteringPage,
      }))
    ),
  },
  {
    path: "/payments/health",
    Component: lazy(() =>
      import("./pages/payments/health").then((m) => ({
        default: m.PaymentHealthPage,
      }))
    ),
  },
  {
    path: "/payments/cycles",
    Component: lazy(() =>
      import("./pages/payments/attempts").then((m) => ({
        default: m.CyclesPage,
      }))
    ),
  },
  {
    path: "/catalog/prices",
    Component: lazy(() =>
      import("./pages/catalog").then((m) => ({ default: m.CatalogPricesPage }))
    ),
  },
  {
    path: "/subscriptions",
    Component: lazy(() =>
      import("./pages/subscriptions").then((m) => ({
        default: m.SubscriptionsPage,
      }))
    ),
  },
  {
    path: "/customers",
    Component: lazy(() =>
      import("./pages/customers").then((m) => ({ default: m.CustomersPage }))
    ),
  },
  {
    path: "/payments",
    Component: lazy(() =>
      import("./pages/payments").then((m) => ({ default: m.PaymentsPage }))
    ),
  },
  {
    path: "/settings",
    Component: lazy(() =>
      import("./pages/settings").then((m) => ({ default: m.SettingsPage }))
    ),
  },
  {
    path: "/reports",
    Component: lazy(() =>
      import("./pages/dashboard/reports").then((m) => ({
        default: m.ReportsPage,
      }))
    ),
  },
  {
    path: "/catalog",
    Component: lazy(() =>
      import("./pages/catalog").then((m) => ({
        default: m.CatalogProductsPage,
      }))
    ),
  },
  {
    path: "/orders",
    Component: lazy(() =>
      import("./pages/orders").then((m) => ({ default: m.OrdersPage }))
    ),
  },
  {
    path: "/ops/imports",
    Component: lazy(() =>
      import("./pages/imports").then((m) => ({ default: m.ImportsPage }))
    ),
  },
  {
    path: "/ops/imports/:id",
    Component: lazy(() =>
      import("./pages/imports/detail").then((m) => ({
        default: m.ImportDetailPage,
      }))
    ),
  },
  {
    path: "/ops/imports/:id/records",
    Component: lazy(() =>
      import("./pages/imports/records").then((m) => ({
        default: m.ImportRecordsPage,
      }))
    ),
  },
  {
    path: "/ops/provider-prices",
    Component: lazy(() =>
      import("./pages/provider-prices").then((m) => ({
        default: m.ProviderPricesPage,
      }))
    ),
  },
  {
    path: "/ops/proposals",
    Component: lazy(() =>
      import("./pages/proposals").then((m) => ({ default: m.ProposalsPage }))
    ),
  },
  {
    path: "/ops/parked",
    Component: lazy(() =>
      import("./pages/parked").then((m) => ({ default: m.ParkedPage }))
    ),
  },
  {
    path: "/ops",
    Component: lazy(() =>
      import("./pages/ops").then((m) => ({ default: m.OpsPage }))
    ),
  },
  {
    path: "/",
    Component: lazy(() =>
      import("./pages/dashboard").then((m) => ({ default: m.DashboardPage }))
    ),
  },
  {
    path: "/customers/:customerId",
    Component: lazy(() =>
      import("./pages/customers/detail").then((m) => ({
        default: m.CustomerDetailPage,
      }))
    ),
  },
  {
    path: "/payments/attempts/:id",
    Component: lazy(() =>
      import("./pages/payments/attempts").then((m) => ({
        default: m.AttemptDetailPage,
      }))
    ),
  },
  {
    path: "/catalog/metering/:key",
    Component: lazy(() =>
      import("./pages/catalog/metering").then((m) => ({
        default: m.MeterDetailPage,
      }))
    ),
  },
  {
    path: "/payments/cycles/:id",
    Component: lazy(() =>
      import("./pages/payments/attempts").then((m) => ({
        default: m.CycleDetailPage,
      }))
    ),
  },
  {
    path: "/catalog/prices/:id",
    Component: lazy(() =>
      import("./pages/catalog/price-detail").then((m) => ({
        default: m.PriceDetailPage,
      }))
    ),
  },
  {
    path: "/subscriptions/:id",
    Component: lazy(() =>
      import("./pages/subscriptions/detail").then((m) => ({
        default: m.SubscriptionDetailPage,
      }))
    ),
  },
  {
    path: "/payments/:id",
    Component: lazy(() =>
      import("./pages/payments/detail").then((m) => ({
        default: m.PaymentDetailPage,
      }))
    ),
  },
  {
    path: "/orders/:id",
    Component: lazy(() =>
      import("./pages/orders/detail").then((m) => ({
        default: m.OrderDetailPage,
      }))
    ),
  },
]

export function matchAdminRoute(pathname: string) {
  for (const { path, Component } of routes) {
    const actual = pathname.replace(/\/$/, "").split("/")
    const expected = path === "/" ? [""] : path.split("/")
    const params: Record<string, string> = {}
    if (actual.length !== expected.length) continue
    if (
      !expected.every((part, i) => {
        if (part.startsWith(":")) {
          try {
            params[part.slice(1)] = decodeURIComponent(actual[i])
            return true
          } catch {
            return false
          }
        }
        return part === actual[i]
      })
    )
      continue
    return { Component, params }
  }
}
export function AdminPages() {
  const match = matchAdminRoute(useLocation().pathname)
  if (!match)
    return (
      <p role="status">
        <AdminMessage id={"This billing page does not exist."} />
      </p>
    )
  return (
    <RouteParams params={match.params}>
      <Suspense
        fallback={
          <p role="status">
            <AdminMessage id={"Loading…"} />
          </p>
        }
      >
        <match.Component />
      </Suspense>
    </RouteParams>
  )
}
