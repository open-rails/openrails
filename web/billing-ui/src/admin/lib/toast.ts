import { toast as sonnerToast } from "sonner"

import { ApiError } from "#orck/admin/lib/api/client"
import { errorText } from "#orck/admin/lib/error-text"

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
      if (err.code === "person_required") {
        toast.error(`${action}: only a signed-in person can do this`, {
          description: err.message,
        })
        return
      }
    }
    // A plain sentence; the API's code stays in the secondary line for
    // support, never in the title.
    const { text, code } = errorText(err)
    toast.error(
      `Couldn't ${action.charAt(0).toLowerCase()}${action.slice(1)}`,
      {
        description: code ? `${text} Code: ${code}.` : text,
      }
    )
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
