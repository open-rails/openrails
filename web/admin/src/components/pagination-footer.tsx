import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon, ArrowRight01Icon } from "@hugeicons/core-free-icons"
import { Button } from "@/components/ui/button"

export function PaginationFooter({
  total,
  limit,
  offset,
  loading,
  onChange,
}: {
  total: number
  limit: number
  offset: number
  loading: boolean
  onChange: (offset: number) => void
}) {
  if (total <= limit) return null
  return (
    <div className="flex items-center justify-between text-sm text-muted-foreground">
      <span className="tabular-nums">
        {offset + 1}–{Math.min(offset + limit, total)} of {total}
      </span>
      <div className="flex gap-1">
        <Button
          variant="ghost"
          size="icon"
          aria-label="Previous page"
          disabled={offset <= 0 || loading}
          onClick={() => onChange(Math.max(0, offset - limit))}
        >
          <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
        </Button>
        <Button
          variant="ghost"
          size="icon"
          aria-label="Next page"
          disabled={offset + limit >= total || loading}
          onClick={() => onChange(offset + limit)}
        >
          <HugeiconsIcon icon={ArrowRight01Icon} className="size-4" />
        </Button>
      </div>
    </div>
  )
}
