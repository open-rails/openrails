import * as React from "react"

// useCursorPages walks a cursor list: the cursor of the page on screen, and
// the trail back to the first page.
export function useCursorPages() {
  const [trail, setTrail] = React.useState<string[]>([])
  return {
    cursor: trail.at(-1),
    page: trail.length + 1,
    next: (cursor: string) => setTrail((t) => [...t, cursor]),
    previous: () => setTrail((t) => t.slice(0, -1)),
    reset: () => setTrail([]),
  }
}
