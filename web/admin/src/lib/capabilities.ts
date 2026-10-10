import { useQuery } from "@tanstack/react-query"

import type { AdminAccess } from "@/lib/api/generated/wire"
import { adminQueries } from "@/lib/queries"

// useAccess is what the signed-in staff member may use of each staff route
// group mounted here (GET /v1/admin/access): the console's four areas.
// Undefined while it loads.
export function useAccess(): AdminAccess | undefined {
  return useQuery(adminQueries.access()).data
}

// hasAnyArea reports a caller who may use at least one area; one who may use
// none sees only that they have no access.
export function hasAnyArea(access: AdminAccess): boolean {
  return (
    access.admin !== "none" ||
    access.catalog ||
    access.merchant_config ||
    access.metrics
  )
}

// useAdminArea reports customer support: customers, subscriptions, payments,
// invoices and findings.
export function useAdminArea(): boolean {
  return (useAccess()?.admin ?? "none") !== "none"
}

// useAdminUpdates reports whether the caller may change customers' billing;
// without it customer support is read-only.
export function useAdminUpdates(): boolean {
  return useAccess()?.admin === "update"
}

// useCatalogArea reports the catalog area: its reads and its edits.
export function useCatalogArea(): boolean {
  return useAccess()?.catalog ?? false
}

// useMerchantConfigEdits reports whether that configuration can change here:
// Vault holds it. Read from a file it is read-only and its edit routes are not
// mounted, so the console offers no edits.
export function useMerchantConfigEdits(): boolean {
  const { data } = useQuery(adminQueries.config())
  return data?.capabilities.features?.merchant_config_edits ?? false
}

// useCatalogWrites reports whether the caller may edit the catalog: the
// catalog area is theirs.
export function useCatalogWrites(): boolean {
  return useCatalogArea()
}

// useMerchantConfig reports the merchant's own configuration: PSPs, settings,
// notifications.
export function useMerchantConfig(): boolean {
  return useAccess()?.merchant_config ?? false
}

// useMetrics reports business metrics: the dashboard, its widgets, the Ops
// gauges.
export function useMetrics(): boolean {
  return useAccess()?.metrics ?? false
}

// useDashboardLayout reports whether the caller may edit the dashboard's
// layout: it shows metrics and is the merchant's configuration.
export function useDashboardLayout(): boolean {
  const access = useAccess()
  return (access?.metrics && access.merchant_config) ?? false
}

// configTabs are the settings tabs that are the merchant's configuration.
const configTabs = ["merchant", "notifications", "psps"]

// adminTabs are the settings tabs that are customer support's.
const adminTabs = ["customer-controls"]

// settingsTab is the settings tab shown for the requested one: the first tab
// when none is requested, the requested one is not shown, or the tabs are not
// the caller's: the configuration tabs need MerchantConfig, customer controls
// customer support. hosted are the host extensions' tabs.
export function settingsTab(
  requested: string | null,
  config: boolean,
  admin: boolean,
  hosted: string[] = []
) {
  const shown = [
    ...(config ? configTabs : []),
    ...(admin ? adminTabs : []),
    ...hosted,
  ]
  const first = shown[0] ?? "merchant"
  if (!requested || !shown.includes(requested)) return first
  return requested
}
