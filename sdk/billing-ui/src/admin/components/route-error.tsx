import { AdminMessage } from "#orck/admin/ui-context"
// A page that failed to render or load. After a deploy the hashed chunk an
// open tab asks for is gone; only a document reload fetches the new
// index.html, so that case offers a reload instead of a broken page.
import { HugeiconsIcon } from "@hugeicons/react"
import { Refresh01Icon } from "@hugeicons/core-free-icons"
import { Link } from "#orck/admin/router"

import { Button } from "#orck/admin/components/ui/button"
import { isChunkLoadError } from "#orck/admin/lib/chunk-error"

export function RouteError({ error }: { error: unknown }) {
  const stale = isChunkLoadError(error)
  const message = error instanceof Error ? error.message : ""
  return (
    <div
      role="alert"
      className="flex min-h-[50vh] flex-col items-center justify-center gap-4 p-6 text-center"
    >
      <div className="max-w-md">
        <p className="font-medium">
          {stale
            ? "The console has been updated. Reload to use the new version."
            : "This page failed to load."}
        </p>
        {!stale && message ? (
          <p className="mt-1 text-sm break-words text-muted-foreground">
            {message}
          </p>
        ) : null}
      </div>
      <div className="flex flex-wrap items-center justify-center gap-3">
        <Button onClick={() => window.location.reload()}>
          <HugeiconsIcon icon={Refresh01Icon} className="size-4" />
          {stale ? "Reload" : "Try again"}
        </Button>
        {/* A document load, not client-side navigation: a stale chunk keeps
            failing until index.html is fetched again. */}
        <Link
          to="/"
          className="text-sm text-muted-foreground underline underline-offset-4"
        >
          <AdminMessage id={"Console home"} />
        </Link>
      </div>
    </div>
  )
}
