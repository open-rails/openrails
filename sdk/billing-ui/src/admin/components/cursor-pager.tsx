import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
// Pager for a cursor list: which rows are on screen (announced), rows per
// page, and first, previous and next. A single page needs no pager.
import { HugeiconsIcon } from "@hugeicons/react"
import {
  ArrowLeft01Icon,
  ArrowLeftDoubleIcon,
  ArrowRight01Icon,
} from "@hugeicons/core-free-icons"

import { Button } from "#orck/admin/components/ui/button"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "#orck/admin/components/ui/select"
import {
  PAGE_SIZES,
  shownRange,
  type CursorPages,
} from "#orck/admin/lib/cursor-pages"

export function CursorPager({
  pages,
  rows,
  nextCursor,
  busy,
  sizes = PAGE_SIZES,
}: {
  pages: CursorPages
  // rows is how many rows the page on screen holds.
  rows: number
  nextCursor: string | null | undefined
  busy?: boolean
  sizes?: number[]
}) {
  const { text: adminText } = useAdminUI()

  if (pages.page === 0 && !nextCursor) return null
  const choices = sizes.includes(pages.size)
    ? sizes
    : [...sizes, pages.size].sort((a, b) => a - b)
  return (
    <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-2 text-sm text-muted-foreground">
      <p className="tabular-nums" aria-live="polite">
        {shownRange(pages.page, pages.size, rows)}
      </p>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <span>
            <AdminMessage id={"Rows per page"} />
          </span>
          <Select
            items={choices.map((n) => ({ value: String(n), label: String(n) }))}
            value={String(pages.size)}
            onValueChange={(value) => {
              if (value) pages.setSize(Number(value))
            }}
          >
            <SelectTrigger size="sm" aria-label={adminText("Rows per page")}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {choices.map((n) => (
                <SelectItem key={n} value={String(n)}>
                  {n}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="flex gap-1">
          <Button
            variant="ghost"
            size="icon"
            aria-label={adminText("First page")}
            disabled={pages.page === 0 || busy}
            onClick={pages.first}
          >
            <HugeiconsIcon icon={ArrowLeftDoubleIcon} className="size-4" />
          </Button>
          <Button
            variant="ghost"
            size="icon"
            aria-label={adminText("Previous page")}
            disabled={pages.page === 0 || busy}
            onClick={pages.previous}
          >
            <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
          </Button>
          <Button
            variant="ghost"
            size="icon"
            aria-label={adminText("Next page")}
            disabled={!nextCursor || busy}
            onClick={() => nextCursor && pages.next(nextCursor)}
          >
            <HugeiconsIcon icon={ArrowRight01Icon} className="size-4" />
          </Button>
        </div>
      </div>
    </div>
  )
}
