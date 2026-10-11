import { toast as sonnerToast } from "sonner"

import { ApiError } from "#orck/admin/lib/api/client"

export function createAdminToast(namespace?: string) {
  const scoped =
    (send: typeof sonnerToast.error) =>
    (
      message: Parameters<typeof send>[0],
      options?: Parameters<typeof send>[1]
    ) =>
      send(message, { ...options, toasterId: namespace })
  return Object.assign(scoped(sonnerToast), {
    error: scoped(sonnerToast.error),
    success: scoped(sonnerToast.success),
    info: scoped(sonnerToast.info),
    warning: scoped(sonnerToast.warning),
  })
}

export function createToastHandlers(
  toast: ReturnType<typeof createAdminToast>
) {
  // toastApiError renders the unified internal/api error envelope consistently.
  // 403s read as a role/permission gap (the API is the authority on
  // permissions — the UI gates on its answers rather than duplicating RBAC).
  function toastApiError(err: unknown, action: string) {
    if (err instanceof ApiError) {
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

  // toastStaleEdit answers an edit refused with revision_mismatch: the caller
  // closes the form and the reload shows what changed.
  function toastStaleEdit(what: string) {
    toast.error(`${what} changed since you opened it`, {
      description: "Reloaded. Review the current version and edit again.",
    })
  }

  return { toastApiError, toastStaleEdit }
}
