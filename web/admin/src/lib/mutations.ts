import { mutationOptions, type QueryClient } from "@tanstack/react-query"

import {
  askCatalogCopilot,
  confirmCopilotDraft,
  type CreatePriceDraft,
} from "@/lib/api/copilot"
import {
  activatePrice,
  activateProduct,
  cancelReprice,
  changeTeamRole,
  createAlertRule,
  createApiKey,
  createPrice,
  createProduct,
  createWebhook,
  deactivatePrice,
  deactivateProduct,
  deleteAlertRule,
  deletePaymentProvider,
  deleteWebhook,
  getCreditLimit,
  getPriceByKey,
  getProduct,
  getTrustLevel,
  inviteTeamMember,
  putMerchantSettings,
  putPaymentProvider,
  previewRepriceAllPriorVersions,
  publishCatalog,
  refreshCatalogDrift,
  removeTeamMember,
  repriceAllPriorVersions,
  revokeApiKey,
  revokeTeamInvite,
  setCreditLimit,
  testAlertRule,
  updateAlertRule,
  updateProduct,
  type AlertRuleRequest,
  type PriceRequest,
  type ProductRequest,
  type UpsertProviderRequest,
  type WebhookRequest,
} from "@/lib/api/endpoints"
import type { MerchantSettings } from "@/lib/api/types"
import { queryKeys } from "@/lib/queries"

const invalidateExact = (
  queryClient: QueryClient,
  queryKey: readonly unknown[]
) => queryClient.invalidateQueries({ queryKey, exact: true })

const invalidateExactOnSuccess =
  (queryClient: QueryClient, queryKey: readonly unknown[]) => () =>
    invalidateExact(queryClient, queryKey)

const invalidateTreeOnSuccess =
  (queryClient: QueryClient, queryKey: readonly unknown[]) => () =>
    queryClient.invalidateQueries({ queryKey })

export const adminMutations = {
  askCatalogCopilot: () =>
    mutationOptions({
      mutationKey: [...queryKeys.catalog(), "copilot", "ask"],
      mutationFn: (question: string) => askCatalogCopilot(question),
    }),
  loadCatalogPriceDraft: () =>
    mutationOptions({
      mutationKey: [...queryKeys.catalog(), "copilot", "load-price-draft"],
      mutationFn: async (priceKey: string) => {
        const price = await getPriceByKey(priceKey)
        const product = await getProduct(price.product_id)
        return { price, productName: product.display_name }
      },
    }),
  createCatalogDraftPrice: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.catalog(), "copilot", "create-price"],
      mutationFn: async ({
        draftId,
        price,
      }: {
        draftId: string
        price: CreatePriceDraft
      }) => {
        const created = await createPrice(price)
        void confirmCopilotDraft(draftId, "catalog_diff", price.key).catch(
          () => {}
        )
        return created
      },
      onSuccess: invalidateTreeOnSuccess(queryClient, queryKeys.catalog()),
    }),
  publishCatalog: (queryClient: QueryClient) => {
    const catalogKey = queryKeys.catalog()
    return mutationOptions({
      mutationKey: [...catalogKey, "publish"],
      mutationFn: ({
        manifest,
        planOnly,
      }: {
        manifest: unknown
        planOnly: boolean
      }) =>
        publishCatalog(
          manifest,
          planOnly ? { plan_only: true } : { insert: true, overwrite: true }
        ),
      onSuccess: (_result, { planOnly }) => {
        if (!planOnly) {
          return queryClient.invalidateQueries({
            queryKey: catalogKey,
          })
        }
      },
    })
  },
  refreshCatalogDrift: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.catalogDrift(), "refresh"],
      mutationFn: () => refreshCatalogDrift(),
      onSuccess: invalidateTreeOnSuccess(queryClient, queryKeys.catalogDrift()),
    }),
  createProduct: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.catalog(), "products", "create"],
      mutationFn: (product: ProductRequest) => createProduct(product),
      onSuccess: invalidateTreeOnSuccess(queryClient, queryKeys.catalog()),
    }),
  updateProduct: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.catalog(), "products", "update"],
      mutationFn: ({
        id,
        product,
      }: {
        id: string
        product: Partial<ProductRequest> & { set_entitlements?: boolean }
      }) => updateProduct(id, product),
      onSuccess: invalidateTreeOnSuccess(queryClient, queryKeys.catalog()),
    }),
  setProductActive: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.catalog(), "products", "set-active"],
      mutationFn: ({ id, active }: { id: string; active: boolean }) =>
        active ? activateProduct(id) : deactivateProduct(id),
      onSuccess: invalidateTreeOnSuccess(queryClient, queryKeys.catalog()),
    }),
  createPrice: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.catalog(), "prices", "create"],
      mutationFn: (price: PriceRequest) => createPrice(price),
      onSuccess: invalidateTreeOnSuccess(queryClient, queryKeys.catalog()),
    }),
  setPriceActive: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.catalog(), "prices", "set-active"],
      mutationFn: ({ id, active }: { id: string; active: boolean }) =>
        active ? activatePrice(id) : deactivatePrice(id),
      onSuccess: invalidateTreeOnSuccess(queryClient, queryKeys.catalog()),
    }),
  previewPriceChange: () =>
    mutationOptions({
      mutationKey: [...queryKeys.catalog(), "prices", "preview-change"],
      mutationFn: (priceKey: string) =>
        previewRepriceAllPriorVersions(priceKey),
    }),
  changePrice: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.catalog(), "prices", "change"],
      mutationFn: async ({
        price,
        migration,
      }: {
        price: PriceRequest
        migration?: { priceKey: string; effectiveAt: string }
      }) => {
        const created = await createPrice(price)
        if (migration) {
          await repriceAllPriorVersions(
            migration.priceKey,
            migration.effectiveAt
          )
        }
        return created
      },
      // The price can be created before scheduling fails. Always refresh so
      // the UI reflects that partial server-side success.
      onSettled: invalidateTreeOnSuccess(queryClient, queryKeys.catalog()),
    }),
  cancelReprices: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.catalog(), "reprices", "cancel"],
      mutationFn: (repriceIds: string[]) =>
        Promise.all(repriceIds.map((id) => cancelReprice(id))),
      // A batch can be partially canceled before one request fails.
      onSettled: invalidateTreeOnSuccess(queryClient, queryKeys.catalog()),
    }),
  updateMerchantSettings: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.settings(), "update"],
      mutationFn: (settings: MerchantSettings) => putMerchantSettings(settings),
      onSuccess: invalidateExactOnSuccess(queryClient, queryKeys.settings()),
    }),
  savePaymentProvider: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.settings(), "payment-providers", "save"],
      mutationFn: ({
        rail,
        provider,
      }: {
        rail: string
        provider: UpsertProviderRequest
      }) => putPaymentProvider(rail, provider),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...queryKeys.settings(),
        "payment-providers",
      ]),
    }),
  archivePaymentProvider: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.settings(), "payment-providers", "archive"],
      mutationFn: ({
        rail,
        environment,
      }: {
        rail: string
        environment?: string
      }) => deletePaymentProvider(rail, environment),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...queryKeys.settings(),
        "payment-providers",
      ]),
    }),
  createApiKey: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.settings(), "api-keys", "create"],
      mutationFn: ({ name, role }: { name: string; role: string }) =>
        createApiKey(name, role),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...queryKeys.settings(),
        "api-keys",
      ]),
    }),
  revokeApiKey: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.settings(), "api-keys", "revoke"],
      mutationFn: (id: string) => revokeApiKey(id),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...queryKeys.settings(),
        "api-keys",
      ]),
    }),
  inviteTeamMember: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.team(), "invite"],
      mutationFn: ({ email, role }: { email: string; role: string }) =>
        inviteTeamMember(email, role),
      onSuccess: invalidateTreeOnSuccess(queryClient, queryKeys.team()),
    }),
  revokeTeamInvite: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.team(), "invites", "revoke"],
      mutationFn: (id: string) => revokeTeamInvite(id),
      onSuccess: invalidateTreeOnSuccess(queryClient, queryKeys.team()),
    }),
  changeTeamRole: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.team(), "role"],
      mutationFn: ({ userId, role }: { userId: string; role: string }) =>
        changeTeamRole(userId, role),
      onSuccess: invalidateTreeOnSuccess(queryClient, queryKeys.team()),
    }),
  removeTeamMember: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.team(), "remove"],
      mutationFn: (userId: string) => removeTeamMember(userId),
      onSuccess: invalidateTreeOnSuccess(queryClient, queryKeys.team()),
    }),
  setCreditLimit: () =>
    mutationOptions({
      mutationKey: [
        ...queryKeys.settings(),
        "customer-controls",
        "credit-limit",
      ],
      mutationFn: ({
        customerId,
        currency,
        amount,
      }: {
        customerId: string
        currency: string
        amount: number
      }) => setCreditLimit(customerId, currency, amount),
    }),
  lookupCustomerControls: () =>
    mutationOptions({
      mutationKey: [...queryKeys.settings(), "customer-controls", "lookup"],
      mutationFn: async ({
        customerId,
        currency,
      }: {
        customerId: string
        currency: string
      }) => {
        const [credit, trust] = await Promise.all([
          getCreditLimit(customerId, currency),
          getTrustLevel(customerId, currency),
        ])
        return { credit, trust }
      },
    }),
  createAlertRule: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.alerts(), "rules", "create"],
      mutationFn: (rule: AlertRuleRequest) => createAlertRule(rule),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...queryKeys.alerts(),
        "rules",
      ]),
    }),
  updateAlertRule: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.alerts(), "rules", "update"],
      mutationFn: ({
        id,
        rule,
      }: {
        id: string
        rule: Partial<AlertRuleRequest>
      }) => updateAlertRule(id, rule),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...queryKeys.alerts(),
        "rules",
      ]),
    }),
  deleteAlertRule: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.alerts(), "rules", "delete"],
      mutationFn: (id: string) => deleteAlertRule(id),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...queryKeys.alerts(),
        "rules",
      ]),
    }),
  testAlertRule: () =>
    mutationOptions({
      mutationKey: [...queryKeys.alerts(), "rules", "test"],
      mutationFn: (id: string) => testAlertRule(id),
    }),
  createWebhook: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.alerts(), "webhooks", "create"],
      mutationFn: (webhook: WebhookRequest) => createWebhook(webhook),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...queryKeys.alerts(),
        "webhooks",
      ]),
    }),
  deleteWebhook: (queryClient: QueryClient) =>
    mutationOptions({
      mutationKey: [...queryKeys.alerts(), "webhooks", "delete"],
      mutationFn: (id: string) => deleteWebhook(id),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...queryKeys.alerts(),
        "webhooks",
      ]),
    }),
}
