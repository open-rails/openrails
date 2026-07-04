import { toast } from "sonner"

import { ApiError } from "@/lib/api/client"

// toastApiError renders the unified pkg/api error envelope consistently.
// 403s read as a role/permission gap (the API is the authority on
// permissions — the UI gates on its answers rather than duplicating RBAC).
export function toastApiError(err: unknown, action: string) {
  if (err instanceof ApiError) {
    if (err.isPermissionDenied) {
      toast.error(`${action}: your role lacks permission`, { description: err.message })
      return
    }
    const label = err.code ? `${action} (${err.code})` : action
    toast.error(label, { description: err.message })
    return
  }
  toast.error(action, { description: err instanceof Error ? err.message : String(err) })
}
