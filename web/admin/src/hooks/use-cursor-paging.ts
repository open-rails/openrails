// useUrlCursor pages a cursor list from the URL: the page's cursor is in
// `cursor`, the earlier cursors in the history entry, so Back and reload land
// on the same page. A link without that history goes Previous to the first
// page. Dropping `cursor` from the URL (a filter change) starts over.
import { useLocation, useSearchParams } from "react-router-dom"

import type { CursorPages } from "@/lib/cursor-pages"

export function useUrlCursor(): CursorPages {
  const [params, setParams] = useSearchParams()
  const state = useLocation().state as { cursors?: string[] } | null
  const cursor = params.get("cursor") ?? ""
  const earlier = state?.cursors ?? []
  const go = (to: string, cursors: string[]) => {
    const p = new URLSearchParams(params)
    if (to) p.set("cursor", to)
    else p.delete("cursor")
    setParams(p, { state: { cursors } })
  }
  return {
    cursor,
    page: cursor ? Math.max(earlier.length, 1) : 0,
    next: (to) => go(to, [...earlier, cursor]),
    previous: () => go(earlier.at(-1) ?? "", earlier.slice(0, -1)),
  }
}
