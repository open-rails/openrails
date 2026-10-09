import { useQuery } from "@tanstack/react-query"

import { adminQueries } from "@/lib/queries"

// useMerchantConfig reports whether the host mounts the merchant's own
// configuration (Routes.MerchantConfig): PSPs, settings, catalog edits,
// billing import and export, the dashboard layout. The console shows those
// pages only then.
export function useMerchantConfig(): boolean {
  const { data } = useQuery(adminQueries.config())
  return data?.capabilities.route_groups?.merchant_config ?? false
}

// configTabs are the settings tabs that are the merchant's configuration.
const configTabs = ["merchant", "notifications", "psps"]

// settingsTab is the settings tab shown for the requested one: the first tab
// when none is requested or the configuration tabs are not mounted.
export function settingsTab(requested: string | null, config: boolean) {
  const first = config ? "merchant" : "team"
  if (!requested || (!config && configTabs.includes(requested))) return first
  return requested
}
