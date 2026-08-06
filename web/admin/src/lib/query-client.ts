import { QueryCache, QueryClient } from "@tanstack/react-query"

import { ApiError } from "@/lib/api/client"
import { toastApiError } from "@/lib/toast"

export function shouldRetry(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError && error.status < 500) return false
  return failureCount < 2
}

export const queryClient = new QueryClient({
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
