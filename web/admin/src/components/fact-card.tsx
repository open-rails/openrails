import * as React from "react"

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"

// Fact renders one labeled stat tile — shared across detail pages
// (subscriptions, payments, catalog prices) so the "grid of facts" layout
// reads identically everywhere.
export function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Card>
      <CardHeader className="pb-1">
        <CardTitle className="text-xs font-normal text-muted-foreground uppercase">{label}</CardTitle>
      </CardHeader>
      <CardContent className="text-sm">{children}</CardContent>
    </Card>
  )
}
