import { useAdminServices } from "#orck/admin/context"
import * as React from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { ApiError } from "#orck/admin/lib/api/client"
import type {
  MerchantConfigurationState,
  UpdateMerchantConfigurationParams,
} from "#orck/client/generated/wire"

type Change = Omit<UpdateMerchantConfigurationParams, "expected_revision">

// useConfigurationEdit edits the merchant's configuration at the revision it
// held when the edit opened, so a change made since is refused instead of
// overwritten; a refused edit closes and the configuration reloads.
export function useConfigurationEdit(
  configuration: MerchantConfigurationState | undefined
) {
  const { toast, toastApiError, toastStaleEdit, adminMutations } =
    useAdminServices()

  const [revision, setRevision] = React.useState<number | null>(null)
  const queryClient = useQueryClient()
  const update = useMutation(
    adminMutations.updateMerchantConfiguration(queryClient)
  )
  return {
    editing: revision !== null,
    open: () => {
      if (configuration) setRevision(configuration.revision)
    },
    close: () => setRevision(null),
    save: async (change: Change, saved: string, action: string) => {
      if (revision === null) return
      try {
        await update.mutateAsync({ ...change, expected_revision: revision })
        toast.success(saved)
        setRevision(null)
      } catch (err) {
        if (err instanceof ApiError && err.revisionMismatch) {
          toastStaleEdit("The configuration")
          setRevision(null)
          return
        }
        toastApiError(err, action)
        if (err instanceof ApiError && err.merchantConfigReadOnly)
          setRevision(null)
      }
    },
  }
}
