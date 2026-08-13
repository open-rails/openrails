// A table row that behaves like the link inside it: the whole row is a target,
// which is how anyone actually clicks a list. The link in the primary cell
// stays, because that is what keyboard and assistive navigation follow.
import * as React from "react"
import { useNavigate } from "react-router-dom"

import { TableRow } from "@/components/ui/table"
import { isInteractiveTarget } from "@/lib/dom"
import { cn } from "@/lib/utils"

export function LinkedTableRow({
  to,
  className,
  children,
  ...props
}: React.ComponentProps<typeof TableRow> & { to: string }) {
  const navigate = useNavigate()
  const follow = (target: EventTarget | null) => {
    if (isInteractiveTarget(target)) return
    navigate(to)
  }
  return (
    <TableRow
      role="link"
      tabIndex={0}
      onClick={(event) => follow(event.target)}
      onKeyDown={(event) => {
        if (event.key !== "Enter") return
        follow(event.target)
      }}
      className={cn(
        "cursor-pointer focus-visible:bg-muted/50 focus-visible:outline-none",
        className
      )}
      {...props}
    >
      {children}
    </TableRow>
  )
}
