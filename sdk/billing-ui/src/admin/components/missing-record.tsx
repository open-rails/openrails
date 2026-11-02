import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon } from "@hugeicons/core-free-icons"

import { ApiError } from "#orck/admin/lib/api/client"
import { errorText } from "#orck/admin/lib/error-text"
import { Link } from "#orck/admin/router"
import { useAdminUI } from "#orck/admin/ui-context"

// MissingRecord is a detail page whose record did not load: a heading that
// says so, the reason in a plain sentence (its code apart), and the way back
// to the list. Missing (404, or no record) and failed reads differ only in the
// heading.
export function MissingRecord({
  title,
  error,
  back,
  backLabel,
}: {
  // "Order not found"
  title: string
  error?: unknown
  back: string
  backLabel: string
}) {
  const { text } = useAdminUI()
  const missing = !error || (error instanceof ApiError && error.status === 404)
  const reason = error && !missing ? errorText(error) : undefined
  return (
    <div className="flex flex-col items-start gap-3">
      <Link
        to={back}
        className="inline-flex min-h-10 items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
      >
        <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
        {text(backLabel)}
      </Link>
      <h1 className="text-2xl font-semibold tracking-tight">
        {text(missing ? title : "This page couldn't be loaded")}
      </h1>
      <p className="max-w-prose text-sm text-muted-foreground" role="alert">
        {reason
          ? text(reason.text)
          : text("It may have been removed, or the link may be wrong.")}
      </p>
      {reason?.code && (
        <p className="text-xs text-muted-foreground">
          {text("Code:")} {reason.code}
        </p>
      )}
    </div>
  )
}
