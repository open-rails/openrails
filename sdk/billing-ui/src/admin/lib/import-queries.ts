import type { AdminClient } from "./api/client"
import type { AdminAPI } from "./api"
import {
  keepPreviousData,
  mutationOptions,
  queryOptions,
  type QueryClient,
} from "@tanstack/react-query"

import { collectCursorPages } from "#orck/admin/lib/api/client"
import type {
  ParkedFilters,
  ProposalFilters,
  ProviderCatalogFilters,
  PSPImportFilters,
  PSPImportRecordFilters,
} from "#orck/admin/lib/api/import-endpoints"
import type {
  CreatePSPImportParams,
  CreateProviderPriceParams,
  LinkPSPImportParams,
  PSPImport,
  PSPImportMapping,
} from "#orck/client/generated/wire"

// An import's job is running: its page polls.
export const importBusy = (status?: PSPImport["status"]) =>
  status === "pulling" || status === "applying"

const POLL_MS = 2_000

const keysOf = (root: () => readonly string[]) => ({
  imports: () => [...root(), "psp-imports"] as const,
  import: (id: string) => [...root(), "psp-imports", id] as const,
  providerCatalog: () => [...root(), "provider-catalog"] as const,
  parked: () => [...root(), "parked"] as const,
  proposals: () => [...root(), "proposals"] as const,
  catalog: () => [...root(), "catalog"] as const,
  ops: () => [...root(), "ops"] as const,
})

export function createImportQueries(client: AdminClient, api: AdminAPI) {
  const { queryRoot } = client
  const importKeys = keysOf(queryRoot)
  // Mutations pin the merchant they started under, as createMutations does.
  const pinnedKeys = () => {
    const root = queryRoot()
    return keysOf(() => root)
  }

  const importQueries = {
    list: (filters: PSPImportFilters, limit: number, cursor?: string) =>
      queryOptions({
        queryKey: [...importKeys.imports(), "list", filters, limit, cursor],
        queryFn: ({ signal }) =>
          api.listPSPImports(filters, { limit, cursor }, signal),
        placeholderData: keepPreviousData,
        refetchInterval: (query) =>
          query.state.data?.data.some((i) => importBusy(i.status))
            ? POLL_MS
            : false,
        meta: { errorAction: "Load PSP imports" },
      }),
    // The PSP's newest import: whose rules a reconcile copies.
    latest: (psp: string) =>
      queryOptions({
        queryKey: [...importKeys.imports(), "latest", psp],
        queryFn: async ({ signal }) =>
          (await api.listPSPImports({ psp }, { limit: 1 }, signal)).data[0] ??
          null,
        enabled: Boolean(psp),
      }),
    detail: (id: string) =>
      queryOptions({
        queryKey: importKeys.import(id),
        queryFn: ({ signal }) => api.getPSPImport(id, signal),
        enabled: Boolean(id),
        refetchInterval: (query) =>
          importBusy(query.state.data?.status) ? POLL_MS : false,
        meta: { errorAction: "Load PSP import" },
      }),
    // version is the import's state: a re-link that finishes reloads them.
    records: (
      id: string,
      filters: PSPImportRecordFilters,
      limit: number,
      cursor?: string,
      version?: string
    ) =>
      queryOptions({
        queryKey: [
          ...importKeys.import(id),
          "records",
          filters,
          limit,
          cursor,
          version,
        ],
        queryFn: ({ signal }) =>
          api.listPSPImportRecords(id, filters, { limit, cursor }, signal),
        enabled: Boolean(id && version),
        placeholderData: keepPreviousData,
        meta: { errorAction: "Load import records" },
      }),
    providerCatalog: (
      filters: ProviderCatalogFilters,
      limit: number,
      cursor?: string
    ) =>
      queryOptions({
        queryKey: [...importKeys.providerCatalog(), filters, limit, cursor],
        queryFn: ({ signal }) =>
          api.listProviderCatalog(filters, { limit, cursor }, signal),
        enabled: Boolean(filters.psp),
        placeholderData: keepPreviousData,
        meta: { errorAction: "Load provider prices" },
      }),
    // One observed price by its ref; the route has no ref filter and a PSP
    // has few prices.
    providerPrice: (psp: string, ref: string) =>
      queryOptions({
        queryKey: [...importKeys.providerCatalog(), "price", psp, ref],
        queryFn: async ({ signal }) =>
          (
            await collectCursorPages(
              (cursor) =>
                api.listProviderCatalog(
                  { psp, kind: "price" },
                  { limit: 500, cursor },
                  signal
                ),
              signal
            )
          ).find((o) => o.ref === ref) ?? null,
        enabled: Boolean(psp && ref),
        meta: { errorAction: "Load provider price" },
      }),
    parked: (filters: ParkedFilters, limit: number, cursor?: string) =>
      queryOptions({
        queryKey: [...importKeys.parked(), filters, limit, cursor],
        queryFn: ({ signal }) =>
          api.listParkedSubscriptions(filters, { limit, cursor }, signal),
        placeholderData: keepPreviousData,
        meta: { errorAction: "Load parked subscriptions" },
      }),
    // Every listed proposal: the inbox groups and selects across pages.
    proposals: (filters: ProposalFilters) =>
      queryOptions({
        queryKey: [...importKeys.proposals(), filters],
        queryFn: ({ signal }) =>
          collectCursorPages(
            (cursor) =>
              api.listProposals(
                filters,
                filters.ids ? {} : { limit: 500, cursor },
                signal
              ),
            signal
          ),
        meta: { errorAction: "Load proposals" },
      }),
    customers: (ids: string[]) =>
      queryOptions({
        queryKey: [...queryRoot(), "customers", "ids", ids],
        queryFn: ({ signal }) => api.customersByIds(ids, signal),
        enabled: ids.length > 0,
      }),
  }

  const importMutations = {
    create: (queryClient: QueryClient) => {
      const keys = pinnedKeys()
      return mutationOptions({
        mutationFn: (params: CreatePSPImportParams) =>
          api.createPSPImport(params),
        onSuccess: () =>
          queryClient.invalidateQueries({ queryKey: keys.imports() }),
      })
    },
    link: (queryClient: QueryClient) => {
      const keys = pinnedKeys()
      return mutationOptions({
        mutationFn: ({
          id,
          params,
        }: {
          id: string
          params: LinkPSPImportParams
        }) => api.linkPSPImport(id, params),
        onSuccess: (_, { id }) =>
          queryClient.invalidateQueries({ queryKey: keys.import(id) }),
      })
    },
    apply: (queryClient: QueryClient) => {
      const keys = pinnedKeys()
      return mutationOptions({
        mutationFn: (id: string) => api.applyPSPImport(id),
        onSuccess: () =>
          queryClient.invalidateQueries({ queryKey: keys.imports() }),
      })
    },
    setMappings: () =>
      mutationOptions({
        mutationFn: (mappings: PSPImportMapping[]) =>
          api.setPSPImportMappings(mappings),
      }),
    // Mapping releases parks and resolves findings on the server.
    mapPrice: (queryClient: QueryClient) => {
      const keys = pinnedKeys()
      return mutationOptions({
        mutationFn: (params: CreateProviderPriceParams) =>
          api.createProviderPrice(params),
        onSuccess: () =>
          Promise.all(
            [
              keys.providerCatalog(),
              keys.parked(),
              keys.imports(),
              keys.catalog(),
              keys.ops(),
            ].map((queryKey) => queryClient.invalidateQueries({ queryKey }))
          ),
      })
    },
    // One approve or dismiss call; the inbox runs a listed set in batches
    // and reloads only when asked, so each row keeps its outcome.
    decide: (queryClient: QueryClient) => {
      const keys = pinnedKeys()
      return mutationOptions({
        mutationFn: ({
          action,
          ids,
          notes,
          key,
        }: {
          action: "approve" | "dismiss"
          ids: string[]
          notes: string
          key: string
        }) =>
          action === "approve"
            ? api.approveProposals(ids, notes, key)
            : api.dismissProposals(ids, notes),
        onSettled: () =>
          Promise.all(
            [keys.imports(), keys.ops()].map((queryKey) =>
              queryClient.invalidateQueries({ queryKey })
            )
          ),
      })
    },
    // dismissPrice ignores an unmapped price's finding: not ours.
    dismissPrice: (queryClient: QueryClient) => {
      const keys = pinnedKeys()
      return mutationOptions({
        mutationFn: ({ finding, notes }: { finding: string; notes: string }) =>
          api.resolveFinding(finding, "ignore", notes),
        onSuccess: () =>
          Promise.all(
            [keys.providerCatalog(), keys.ops()].map((queryKey) =>
              queryClient.invalidateQueries({ queryKey })
            )
          ),
      })
    },
  }

  return { importKeys, importQueries, importMutations }
}
