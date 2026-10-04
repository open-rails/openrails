import * as React from "react"

// useCursorPages is the cursor of a list's visible page: next_cursor pages
// forward, previous pops back through the cursors visited, and a new filter
// key restarts at the first page.
export function useCursorPages(filter: string) {
  const [state, setState] = React.useState({ filter, stack: [] as string[] })
  const stack = state.filter === filter ? state.stack : []
  return {
    cursor: stack.at(-1) ?? "",
    page: stack.length,
    next: (cursor: string) => setState({ filter, stack: [...stack, cursor] }),
    previous: () => setState({ filter, stack: stack.slice(0, -1) }),
  }
}

export type CursorPages = ReturnType<typeof useCursorPages>
