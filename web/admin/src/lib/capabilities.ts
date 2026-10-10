import { useQuery } from "@tanstack/react-query"

import { adminQueries } from "@/lib/queries"

// useMerchantConfig reports whether the host mounts the merchant's own
// configuration (Permissions.MerchantConfig): PSPs, settings, billing import
// and export, the dashboard layout. The console shows those pages only then.
export function useMerchantConfig(): boolean {
  const { data } = useQuery(adminQueries.config())
  return data?.capabilities.route_groups?.merchant_config ?? false
}

// useCatalogWrites reports whether staff may edit the catalog here: the host
// mounts catalog edits (Permissions.CatalogWrite) and the catalog accepts
// them (no catalog file is the truth).
export function useCatalogWrites(): boolean {
  const { data: config } = useQuery(adminQueries.config())
  const { data: revision } = useQuery(adminQueries.catalogRevision())
  return (
    (config?.capabilities.route_groups?.catalog_write ?? false) &&
    revision?.writes_allowed === true
  )
}

// configTabs are the settings tabs that are the merchant's configuration.
const configTabs = ["merchant", "notifications", "psps"]

// The settings tabs shown without the merchant's configuration.
const otherTabs = ["customer-controls"]

// settingsTab is the settings tab shown for the requested one: the first tab
// when none is requested, the requested one is not shown, or the
// configuration tabs are not mounted. hosted are the host extensions' tabs.
export function settingsTab(
  requested: string | null,
  config: boolean,
  hosted: string[] = []
) {
  const first = config ? "merchant" : "customer-controls"
  const shown = [...(config ? configTabs : []), ...otherTabs, ...hosted]
  if (!requested || !shown.includes(requested)) return first
  return requested
}
