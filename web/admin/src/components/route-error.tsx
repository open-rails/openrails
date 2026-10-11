// A page that failed to render or load. After a deploy the hashed chunk an
// open tab asks for is gone; only a document reload fetches the new
// index.html, so that case offers a reload instead of a broken page.
import { HugeiconsIcon } from "@hugeicons/react"
import { Refresh01Icon } from "@hugeicons/core-free-icons"
import { useRouteError } from "react-router-dom"

import { Button } from "@/components/ui/button"
import { isChunkLoadError } from "@/lib/chunk-error"
import { routerBasename } from "@/lib/mount"

export function RouteError() {
  const error = useRouteError()
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
        <a
          href={`${routerBasename().replace(/\/$/, "")}/`}
          className="text-sm text-muted-foreground underline underline-offset-4"
        >
          Console home
        </a>
      </div>
    </div>
  )
}
