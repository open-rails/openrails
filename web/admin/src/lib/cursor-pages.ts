// Paging state for a cursor list: the cursor of the page on screen, the page
// number, the rows per page, and moves. A cursor list has no total, so the
// pager offers first, previous and next, never a last page.
import * as React from "react"
import { useLocation, useSearchParams } from "react-router-dom"

export interface CursorPages {
  // cursor of the page on screen; "" is the first page.
  cursor: string
  // page is 0 on the first page.
  page: number
  size: number
  setSize: (size: number) => void
  next: (cursor: string) => void
  previous: () => void
  first: () => void
}

export const PAGE_SIZES = [25, 50, 100]

// shownRange is the "Showing 51–100" line for the rows on screen.
export function shownRange(page: number, size: number, rows: number): string {
  if (rows === 0) return "No rows"
  const start = page * size + 1
  return `Showing ${start}–${start + rows - 1}`
}

// useCursorPages keeps the position in component state: a new filter key or
// rows-per-page restarts at the first page.
export function useCursorPages(filter: string, defaultSize = 50): CursorPages {
  const [state, setState] = React.useState({
    filter,
    size: defaultSize,
    stack: [] as string[],
  })
  const stack = state.filter === filter ? state.stack : []
  const { size } = state
  return {
    cursor: stack.at(-1) ?? "",
    page: stack.length,
    size,
    setSize: (next) => setState({ filter, size: next, stack: [] }),
    next: (cursor) => setState({ filter, size, stack: [...stack, cursor] }),
    previous: () => setState({ filter, size, stack: stack.slice(0, -1) }),
    first: () => setState({ filter, size, stack: [] }),
  }
}

// useUrlCursor keeps the position in the URL: the page's cursor in `cursor`,
// rows per page in `limit`, the earlier cursors in the history entry, so Back
// and reload land on the same page. A link without that history goes
// Previous to the first page. Dropping `cursor` (a filter change) starts over.
export function useUrlCursor(defaultSize = 50): CursorPages {
  const [params, setParams] = useSearchParams()
  const state = useLocation().state as { cursors?: string[] } | null
  const cursor = params.get("cursor") ?? ""
  const limit = Number(params.get("limit"))
  const size = PAGE_SIZES.includes(limit) ? limit : defaultSize
  const earlier = state?.cursors ?? []
  const go = (to: string, cursors: string[], nextSize = size) => {
    const p = new URLSearchParams(params)
    if (to) p.set("cursor", to)
    else p.delete("cursor")
    if (nextSize === defaultSize) p.delete("limit")
    else p.set("limit", String(nextSize))
    setParams(p, { state: { cursors } })
  }
  return {
    cursor,
    page: cursor ? Math.max(earlier.length, 1) : 0,
    size,
    setSize: (next) => go("", [], next),
    next: (to) => go(to, [...earlier, cursor]),
    previous: () => go(earlier.at(-1) ?? "", earlier.slice(0, -1)),
    first: () => go("", []),
  }
}
