import {
  CreditCardIcon,
  DashboardCircleIcon,
  PackageIcon,
  RepeatIcon,
  Settings01Icon,
  UserGroupIcon,
  Wrench01Icon,
} from "@hugeicons/core-free-icons"
import type { IconSvgElement } from "@hugeicons/react"
import {
  useAdminArea,
  useCatalogArea,
  useMerchantConfig,
  useMeteringArea,
  useMetrics,
} from "./lib/capabilities"
export interface AdminNavItem {
  title: string
  path: string
  icon?: IconSvgElement
  useVisible?: () => boolean
  invoice?: number
  items?: { title: string; path: string; useVisible?: () => boolean }[]
}
export const coreNav: AdminNavItem[] = [
  {
    title: "Dashboard",
    path: "/",
    icon: DashboardCircleIcon,
    invoice: 0,
    useVisible: useMetrics,
    items: [
      { title: "Overview", path: "/" },
      { title: "Reports", path: "/reports" },
    ],
  },
  {
    title: "Customers",
    path: "/customers",
    icon: UserGroupIcon,
    invoice: 10,
    useVisible: useAdminArea,
  },
  {
    title: "Subscriptions",
    path: "/subscriptions",
    icon: RepeatIcon,
    invoice: 20,
    useVisible: useAdminArea,
  },
  {
    title: "Payments",
    path: "/payments",
    icon: CreditCardIcon,
    invoice: 30,
    useVisible: useAdminArea,
    items: [
      { title: "Payments", path: "/payments" },
      { title: "Health", path: "/payments/health", useVisible: useMetrics },
      { title: "Attempts", path: "/payments/attempts" },
      { title: "Renewals", path: "/payments/cycles" },
    ],
  },
  {
    title: "Invoices",
    path: "/invoices",
    icon: CreditCardIcon,
    invoice: 40,
    useVisible: useAdminArea,
  },
  {
    title: "Catalog",
    path: "/catalog",
    icon: PackageIcon,
    invoice: 50,
    useVisible: useCatalogArea,
    items: [
      { title: "Products", path: "/catalog" },
      { title: "Prices", path: "/catalog/prices" },
      {
        title: "Metering",
        path: "/catalog/metering",
        useVisible: useMeteringArea,
      },
    ],
  },
  {
    title: "Ops",
    path: "/ops",
    icon: Wrench01Icon,
    invoice: 60,
    useVisible: useMerchantConfig,
    items: [
      { title: "Findings", path: "/ops" },
      { title: "Signals", path: "/ops/signals", useVisible: useMetrics },
      { title: "Proposals", path: "/ops/proposals" },
      { title: "PSP imports", path: "/ops/imports" },
      { title: "Provider prices", path: "/ops/provider-prices" },
      { title: "Parked", path: "/ops/parked" },
    ],
  },
  { title: "Settings", path: "/settings", icon: Settings01Icon, invoice: 70 },
].map((item) => ({ ...item }))
