import { LayoutDashboardIcon } from "lucide-react"

import {
  Card,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"

// Static placeholder: the real dashboard is a per-merchant widget grid over
// the #733 metrics API, built as #741. Do not wire endpoints here — they
// land on a parallel branch.
export function DashboardPage() {
  return (
    <div className="flex flex-1 items-center justify-center">
      <Card className="max-w-md text-center">
        <CardHeader>
          <div className="mx-auto mb-2 flex size-10 items-center justify-center rounded-lg bg-muted">
            <LayoutDashboardIcon className="size-5 text-muted-foreground" />
          </div>
          <CardTitle>Dashboard widgets are on their way</CardTitle>
          <CardDescription>
            This page becomes a configurable widget grid over the merchant metrics API
            (#733) with saved, per-merchant widgets (#741). Until those land, use the
            Customers, Subscriptions and Payments pages on the left.
          </CardDescription>
        </CardHeader>
      </Card>
    </div>
  )
}
