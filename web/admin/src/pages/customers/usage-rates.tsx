import { HugeiconsIcon } from "@hugeicons/react"
import { Delete02Icon, InformationCircleIcon } from "@hugeicons/core-free-icons"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Link } from "react-router-dom"
import { toast } from "sonner"

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { ApiError } from "@/lib/api/client"
import type { CustomerUsageRateOverride, UsageMeter } from "@/lib/api/types"
import { formatDate } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"
import { toastApiError } from "@/lib/toast"
import {
  customerUsageRateRows,
  summarizeRateCard,
} from "../catalog/metering-model"
import { RateCardEditor } from "../catalog/rate-card-editor"

export function CustomerUsageRatesSection({
  customerId,
}: {
  customerId: string
}) {
  const ratesQuery = useQuery(adminQueries.customerUsageRates(customerId))
  const metersQuery = useQuery(adminQueries.allUsageMeters())
  const rows = customerUsageRateRows(
    metersQuery.data?.items ?? [],
    ratesQuery.data ?? []
  )
  const pending = ratesQuery.isPending || metersQuery.isPending
  const error = ratesQuery.error ?? metersQuery.error
  const forbidden = error instanceof ApiError && error.status === 403

  return (
    <Card className="min-w-0">
      <CardHeader>
        <CardTitle className="text-sm">Negotiated usage rates</CardTitle>
        <p className="text-xs text-muted-foreground">
          Customer-specific usage pricing. Removing a negotiated rate restores
          the merchant default.
        </p>
      </CardHeader>
      <CardContent>
        {pending ? (
          <div
            className="grid gap-2"
            aria-label="Loading negotiated usage rates"
          >
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        ) : error ? (
          <div className="flex items-start gap-2 py-3 text-sm text-muted-foreground">
            <HugeiconsIcon
              icon={InformationCircleIcon}
              className="mt-0.5 size-4 shrink-0"
            />
            <div>
              <p className="font-medium text-foreground">
                {forbidden
                  ? "Usage-rate access is restricted"
                  : "Negotiated rates could not be loaded"}
              </p>
              <p className="mt-1">
                {forbidden
                  ? "Your role cannot read or manage these contracts."
                  : "Try loading this customer again."}
              </p>
            </div>
          </div>
        ) : rows.length === 0 ? (
          <div className="border-y py-6 text-sm text-muted-foreground">
            <p>No metered products are ready for negotiated pricing.</p>
            <Link
              to="/catalog/metering"
              className="mt-2 inline-block text-primary underline underline-offset-3"
            >
              Configure a default usage rate first
            </Link>
          </div>
        ) : (
          <div className="max-w-full overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="text-muted-foreground">Meter</TableHead>
                  <TableHead className="text-muted-foreground">
                    Inherited contract
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    Default rate
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    Customer rate
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    Updated
                  </TableHead>
                  <TableHead>
                    <span className="sr-only">Actions</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map(({ meter, override }) => (
                  <UsageRateRow
                    key={`${meter.key}:${override?.updated_at ?? "default"}`}
                    customerId={customerId}
                    meter={meter}
                    meters={metersQuery.data?.items ?? []}
                    override={override}
                  />
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function UsageRateRow({
  customerId,
  meter,
  meters,
  override,
}: {
  customerId: string
  meter: UsageMeter
  meters: UsageMeter[]
  override?: CustomerUsageRateOverride
}) {
  const defaultCard = meter.default_rate_card!
  return (
    <TableRow>
      <TableCell>
        <Link
          to={`/catalog/metering/${encodeURIComponent(meter.key)}`}
          className="font-medium underline-offset-3 hover:underline"
        >
          {meter.key}
        </Link>
        {meter.unit && (
          <p className="text-xs text-muted-foreground">{meter.unit}</p>
        )}
      </TableCell>
      <TableCell>
        <p className="font-medium">{defaultCard.product_key}</p>
        <p className="max-w-64 text-xs text-muted-foreground">
          {formatFilter(defaultCard.filter)}
        </p>
      </TableCell>
      <TableCell className="tabular-nums">
        {summarizeDefault(defaultCard)}
      </TableCell>
      <TableCell className="tabular-nums">
        {override ? summarizeOverride(override) : "Uses merchant default"}
      </TableCell>
      <TableCell className="text-muted-foreground tabular-nums">
        {override ? formatDate(override.updated_at) : "—"}
      </TableCell>
      <TableCell>
        <div className="flex justify-end gap-1">
          <RateCardEditor
            meter={meter}
            meters={meters}
            products={[]}
            customerId={customerId}
            override={override}
          />
          {override && (
            <RemoveOverrideButton
              customerId={customerId}
              meterKey={meter.key}
            />
          )}
        </div>
      </TableCell>
    </TableRow>
  )
}

function RemoveOverrideButton({
  customerId,
  meterKey,
}: {
  customerId: string
  meterKey: string
}) {
  const queryClient = useQueryClient()
  const remove = useMutation(
    adminMutations.deleteCustomerUsageRateOverride(queryClient)
  )
  const handleRemove = async () => {
    try {
      await remove.mutateAsync({ customerId, meterKey })
      toast.success("Merchant default restored")
    } catch (error) {
      toastApiError(error, "Remove negotiated rate")
    }
  }
  return (
    <AlertDialog>
      <AlertDialogTrigger
        render={
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label={`Remove negotiated rate for ${meterKey}`}
          >
            <HugeiconsIcon icon={Delete02Icon} className="size-4" />
          </Button>
        }
      />
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Remove the negotiated rate?</AlertDialogTitle>
          <AlertDialogDescription>
            This customer will resume the merchant default for {meterKey}.
            Finalized invoices and amounts already accrued remain unchanged.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Keep negotiated rate</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={remove.isPending}
            onClick={() => void handleRemove()}
          >
            {remove.isPending ? "Removing…" : "Restore default rate"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

function summarizeOverride(override: CustomerUsageRateOverride): string {
  const price = summarizeRateCard({
    price: override.price,
  } as NonNullable<UsageMeter["default_rate_card"]>)
  if (override.allowance?.included !== undefined) {
    return `${price} · ${override.allowance.included.toLocaleString()} included`
  }
  if (override.allowance?.accrue_from) {
    return `${price} · allowance from ${override.allowance.accrue_from}`
  }
  return price
}

function summarizeDefault(
  card: NonNullable<UsageMeter["default_rate_card"]>
): string {
  const price = summarizeRateCard(card)
  if (card.allowance?.included !== undefined) {
    return `${price} · ${card.allowance.included.toLocaleString()} included`
  }
  if (card.allowance?.accrue_from) {
    return `${price} · allowance from ${card.allowance.accrue_from}`
  }
  return price
}

function formatFilter(filter: Record<string, string[]>): string {
  const entries = Object.entries(filter)
  if (entries.length === 0) return "All matching events"
  return entries
    .map(([dimension, values]) => `${dimension}: ${values.join(", ")}`)
    .join(" · ")
}
