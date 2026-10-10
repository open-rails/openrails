import { toast } from "sonner"

import { ApiError } from "@/lib/api/client"

// toastApiError renders the unified internal/api error envelope consistently.
// 403s read as a role/permission gap (the API is the authority on
// permissions — the UI gates on its answers rather than duplicating RBAC).
export function toastApiError(err: unknown, action: string) {
  if (err instanceof ApiError) {
    if (err.stepUpRequired) {
      toast.error(`${action}: confirm it's you to continue`)
      return
    }
    if (err.code === "revision_mismatch") {
      toast.error(`${action}: changed since you opened it`, {
        description: "The latest version is loaded; review it and try again.",
      })
      return
    }
    if (err.isPermissionDenied) {
      toast.error(`${action}: your role lacks permission`, {
        description: err.message,
      })
      return
    }
    const label = err.code ? `${action} (${err.code})` : action
    toast.error(label, { description: err.message })
    return
  }
  toast.error(action, {
    description: err instanceof Error ? err.message : String(err),
  })
}
