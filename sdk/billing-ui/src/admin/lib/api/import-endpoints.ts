import type { AdminClient, PageRequest } from "./client"

import type {
  CreatePSPImportParams,
  CreateProviderPriceParams,
  Customer,
  Finding,
  LinkPSPImportParams,
  ListPage,
  PSPImport,
  PSPImportMapping,
  PSPImportMappingsResult,
  PSPImportRecord,
  ParkedSubscription,
  Price,
  ProposalResults,
  ProviderCatalogObject,
} from "#orck/client/generated/wire"

export interface PSPImportFilters {
  // A PSP's id or key.
  psp?: string
  origin?: PSPImport["origin"]
}

export interface PSPImportRecordFilters {
  state?: PSPImportRecord["state"]
  kind?: PSPImportRecord["kind"]
}

// One PSP's observed catalog. import keeps the prices that import's staged
// subscriptions name, each with their count.
export interface ProviderCatalogFilters {
  psp: string
  kind?: ProviderCatalogObject["kind"]
  mapped?: boolean
  retired?: boolean
  import?: string
}

export interface ParkedFilters {
  psp?: string
  reason?: ParkedSubscription["reason"]
  released?: boolean
}

// Open pull.* proposals by default. ids alone names up to 100, whatever
// their state.
export interface ProposalFilters {
  ids?: string
  psp_id?: string
  pull_id?: string
  kind?: string
}

// PSP imports, the observed provider catalog and parked subscriptions: the
// pull, its mapping and what webhooks could not land.
export function createImportEndpoints(client: AdminClient) {
  const { api } = client

  const listPSPImports = (
    filters: PSPImportFilters,
    page: PageRequest,
    signal?: AbortSignal
  ) =>
    api<ListPage<PSPImport>>("/merchant/psp-imports", {
      query: { ...filters, ...page },
      signal,
    })

  const getPSPImport = (id: string, signal?: AbortSignal) =>
    api<PSPImport>(`/merchant/psp-imports/${encodeURIComponent(id)}`, {
      signal,
    })

  // createPSPImport queues a manual pull; the server picks its rules.
  const createPSPImport = (params: CreatePSPImportParams) =>
    api<PSPImport>("/merchant/psp-imports", { method: "POST", body: params })

  const listPSPImportRecords = (
    id: string,
    filters: PSPImportRecordFilters,
    page: PageRequest,
    signal?: AbortSignal
  ) =>
    api<ListPage<PSPImportRecord>>(
      `/merchant/psp-imports/${encodeURIComponent(id)}/records`,
      { query: { ...filters, ...page }, signal }
    )

  // linkPSPImport stores links and price picks, then decides every record not
  // yet imported again; empty params only re-link.
  const linkPSPImport = (id: string, params: LinkPSPImportParams) =>
    api<PSPImport>(`/merchant/psp-imports/${encodeURIComponent(id)}/link`, {
      method: "POST",
      body: params,
    })

  // applyPSPImport queues the landing of the linked records.
  const applyPSPImport = (id: string) =>
    api<PSPImport>(`/merchant/psp-imports/${encodeURIComponent(id)}/apply`, {
      method: "POST",
    })

  const setPSPImportMappings = (mappings: PSPImportMapping[]) =>
    api<PSPImportMappingsResult>("/merchant/psp-import-mappings", {
      method: "POST",
      body: { mappings },
    })

  const listProviderCatalog = (
    filters: ProviderCatalogFilters,
    page: PageRequest,
    signal?: AbortSignal
  ) =>
    api<ListPage<ProviderCatalogObject>>("/merchant/provider-catalog", {
      query: { ...filters, ...page },
      signal,
    })

  const createProviderPrice = (params: CreateProviderPriceParams) =>
    api<Price>("/catalog/provider-prices", { method: "POST", body: params })

  const listParkedSubscriptions = (
    filters: ParkedFilters,
    page: PageRequest,
    signal?: AbortSignal
  ) =>
    api<ListPage<ParkedSubscription>>("/merchant/parked-subscriptions", {
      query: { ...filters, ...page },
      signal,
    })

  const listProposals = (
    filters: ProposalFilters,
    page: PageRequest,
    signal?: AbortSignal
  ) =>
    api<ListPage<Finding>>("/merchant/proposals", {
      query: { ...filters, ...page },
      signal,
    })

  // approveProposals approves up to 100 proposals. The key replays the first
  // answer: a retry of the same batch reuses it.
  const approveProposals = (ids: string[], notes: string, key: string) =>
    api<ProposalResults>("/merchant/proposals/approve", {
      method: "POST",
      headers: { "Idempotency-Key": key },
      body: { ids, notes },
    })

  const dismissProposals = (ids: string[], notes: string) =>
    api<ProposalResults>("/merchant/proposals/dismiss", {
      method: "POST",
      body: { ids, notes },
    })

  // customersByIds reads named customers: only those with billing rows exist.
  const customersByIds = (ids: string[], signal?: AbortSignal) =>
    api<ListPage<Customer>>("/admin/customers", {
      query: { ids: ids.join(",") },
      signal,
    })

  return {
    listPSPImports,
    getPSPImport,
    createPSPImport,
    listPSPImportRecords,
    linkPSPImport,
    applyPSPImport,
    setPSPImportMappings,
    listProviderCatalog,
    createProviderPrice,
    listParkedSubscriptions,
    customersByIds,
    listProposals,
    approveProposals,
    dismissProposals,
  }
}
