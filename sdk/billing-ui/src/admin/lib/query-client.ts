import { QueryCache, QueryClient } from "@tanstack/react-query"

import { ApiError } from "#orck/admin/lib/api/client"
import { createAdminToast, createToastHandlers } from "#orck/admin/lib/toast"

export function shouldRetry(failureCount: number, error: unknown): boolean {
  // Status 0: OpenRails never answered.
  if (error instanceof ApiError && error.status > 0 && error.status < 500)
    return false
  return failureCount < 2
}

export const createAdminQueryClient = (namespace?: string) => {
  const { toastApiError } = createToastHandlers(createAdminToast(namespace))
  return new QueryClient({
    queryCache: new QueryCache({
      onError: (error, query) => {
        const action = query.meta?.errorAction
        if (typeof action === "string") toastApiError(error, action)
      },
    }),
    defaultOptions: {
      queries: {
        staleTime: 20_000,
        gcTime: 5 * 60_000,
        retry: shouldRetry,
      },
      mutations: {
        retry: false,
      },
    },
  })
}
