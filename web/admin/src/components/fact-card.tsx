import * as React from "react"

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"

// Fact renders one labeled stat tile for a detail page's grid of facts.
export function Fact({
  label,
  children,
}: {
  label: string
  children: React.ReactNode
}) {
  return (
    <Card>
      <CardHeader className="pb-1">
        <CardTitle className="text-xs font-normal text-muted-foreground uppercase">
          {label}
        </CardTitle>
      </CardHeader>
      <CardContent className="text-sm">{children}</CardContent>
    </Card>
  )
}
