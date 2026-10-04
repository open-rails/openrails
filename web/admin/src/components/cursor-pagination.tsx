import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon, ArrowRight01Icon } from "@hugeicons/core-free-icons"
import { Button } from "@/components/ui/button"
import type { useCursorPages } from "@/hooks/use-cursor-pages"

export function CursorPaginationFooter({
  pages,
  nextCursor,
  loading,
}: {
  pages: ReturnType<typeof useCursorPages>
  nextCursor: string | null | undefined
  loading: boolean
}) {
  if (pages.page === 1 && !nextCursor) return null
  return (
    <div className="flex items-center justify-between text-sm text-muted-foreground">
      <span className="tabular-nums">Page {pages.page}</span>
      <div className="flex gap-1">
        <Button
          variant="ghost"
          size="icon"
          aria-label="Previous page"
          disabled={pages.page <= 1 || loading}
          onClick={pages.previous}
        >
          <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
        </Button>
        <Button
          variant="ghost"
          size="icon"
          aria-label="Next page"
          disabled={!nextCursor || loading}
          onClick={() => nextCursor && pages.next(nextCursor)}
        >
          <HugeiconsIcon icon={ArrowRight01Icon} className="size-4" />
        </Button>
      </div>
    </div>
  )
}
