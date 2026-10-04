// Pager for a cursor list: Next follows next_cursor, Previous goes back.
import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon, ArrowRight01Icon } from "@hugeicons/core-free-icons"
import { Button } from "@/components/ui/button"
import type { CursorPages } from "@/lib/cursor-pages"

export function CursorPager({
  pages,
  nextCursor,
  busy,
}: {
  pages: CursorPages
  nextCursor: string | null | undefined
  busy?: boolean
}) {
  if (pages.page === 0 && !nextCursor) return null
  return (
    <div className="flex items-center justify-between text-sm text-muted-foreground">
      <span className="tabular-nums">Page {pages.page + 1}</span>
      <div className="flex gap-1">
        <Button
          variant="ghost"
          size="icon"
          aria-label="Previous page"
          disabled={pages.page === 0 || busy}
          onClick={pages.previous}
        >
          <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
        </Button>
        <Button
          variant="ghost"
          size="icon"
          aria-label="Next page"
          disabled={!nextCursor || busy}
          onClick={() => nextCursor && pages.next(nextCursor)}
        >
          <HugeiconsIcon icon={ArrowRight01Icon} className="size-4" />
        </Button>
      </div>
    </div>
  )
}
