import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { toast } from "sonner"
import { useQuery, useQueryClient } from "@tanstack/react-query"

import { Fact } from "@/components/fact-card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { cancelReprice } from "@/lib/api/endpoints"
import { formatDate, formatMicros, shortId } from "@/lib/format"
import { toastApiError } from "@/lib/toast"
import { priceIntervalLabel } from "@/pages/catalog/price-format"
import { PriceChangeWizard } from "@/pages/catalog/price-wizard"
import { CheckoutReadinessCard, PSPLinksCard } from "@/pages/catalog/psp-links"
import { adminQueries, queryKeys } from "@/lib/queries"

// PriceDetailPage (#777): the version chain (from the #774 pointer-movement
// log, with dates) and any pending scheduled migration (progress + cancel)
// for one price key — the wizard's Step 1 entry point ("Change price") also
// lives here alongside the catalog list row.
export function PriceDetailPage() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  // verify (or#812) is opt-in: it makes GET price perform a live retrieve
  // against every attached provider, which is a network round trip per PSP.
  const [verify, setVerify] = React.useState(false)
  const {
    data: price,
    isPending: loading,
    isFetching,
    refetch: reload,
  } = useQuery(adminQueries.price(id, { verify, errorAction: "Load price" }))
  const { data: product } = useQuery(adminQueries.product(price?.product_id))
  const { data: history } = useQuery(adminQueries.priceHistory(price?.key))
  const { data: batches } = useQuery(adminQueries.repriceBatches(price?.key))
  const latestBatch = batches?.items?.[0]
  const { data: batchReprices, refetch: reloadBatchReprices } = useQuery(
    adminQueries.reprices(
      latestBatch ? { reprice_batch_id: latestBatch.id } : undefined
    )
  )

  const reloadAll = () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.catalog() })
  }

  // Only the FIRST load blanks the page; a verify refetch keeps the rendered
  // price in place so the button can show its own in-flight state.
  if (!price)
    return (
      <p className="text-sm text-muted-foreground">
        {loading ? "Loading…" : "Price not found."}
      </p>
    )

  const scheduled =
    batchReprices?.items?.filter((r) => r.status === "scheduled") ?? []
  const applied =
    batchReprices?.items?.filter((r) => r.status === "applied") ?? []
  const isPending = !!latestBatch && scheduled.length > 0

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-3">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => navigate(-1)}
          aria-label="Back"
        >
          <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
        </Button>
        <div>
          <h2 className="flex items-center gap-2 text-sm">
            {price.key}
            {price.archived && <Badge variant="secondary">archived</Badge>}
          </h2>
          <p className="text-xs text-muted-foreground">
            {shortId(price.id, 16)}
          </p>
        </div>
        <div className="ml-auto">
          <PriceChangeWizard
            price={price}
            productName={product?.display_name ?? "…"}
            onDone={reloadAll}
          />
        </div>
      </div>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Fact label="Product">
          <Link className="underline-offset-2 hover:underline" to={`/catalog`}>
            {product?.display_name ?? shortId(price.product_id, 13)}
          </Link>
        </Fact>
        <Fact label="Amount">
          {formatMicros(price.unit_amount, price.currency)}
        </Fact>
        <Fact label="Currency · interval">
          {price.currency.toUpperCase()} · {priceIntervalLabel(price)}
        </Fact>
        <Fact label="Created">{formatDate(price.created_at)}</Fact>
      </div>

      <PSPLinksCard
        price={price}
        verifying={verify && isFetching}
        verified={verify}
        onVerify={() => (verify ? void reload() : setVerify(true))}
      />

      <CheckoutReadinessCard price={price} />

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Version chain</CardTitle>
        </CardHeader>
        <CardContent>
          {!history?.items?.length ? (
            <p className="text-sm text-muted-foreground">No history yet.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Amount</TableHead>
                  <TableHead>Since</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Price</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {history.items.map((entry) => (
                  <TableRow key={`${entry.price.id}-${entry.effective_at}`}>
                    <TableCell>
                      {formatMicros(
                        entry.price.unit_amount,
                        entry.price.currency
                      )}
                    </TableCell>
                    <TableCell>{formatDate(entry.effective_at)}</TableCell>
                    <TableCell>
                      {entry.price.archived ? (
                        <Badge variant="secondary">grandfathered</Badge>
                      ) : (
                        <Badge className="bg-emerald-500/15 text-emerald-600 dark:text-emerald-400">
                          current
                        </Badge>
                      )}
                    </TableCell>
                    <TableCell>
                      <Link
                        className="text-xs underline-offset-2 hover:underline"
                        to={`/catalog/prices/${entry.price.id}`}
                      >
                        {shortId(entry.price.id, 13)}
                      </Link>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {latestBatch && (
        <Card>
          <CardHeader className="flex flex-row items-center justify-between gap-2">
            <CardTitle className="text-sm">
              {isPending ? "Pending migration" : "Last migration"}
            </CardTitle>
            {isPending && (
              <CancelMigrationButton
                repriceIds={scheduled.map((r) => r.id)}
                onDone={() => {
                  reloadBatchReprices()
                  reloadAll()
                }}
              />
            )}
          </CardHeader>
          <CardContent className="flex flex-col gap-2 text-sm">
            <p>
              {applied.length} of {latestBatch.subscriptions_scheduled} migrated
              {scheduled.length > 0 && ` · ${scheduled.length} still scheduled`}
              {" · effective "}
              {formatDate(latestBatch.effective_at)}
            </p>
            <p className="text-xs text-muted-foreground">
              {latestBatch.subscriptions_skipped > 0 &&
                `${latestBatch.subscriptions_skipped} subscription(s) were skipped (constraint violation or existing schedule conflict) at schedule time. `}
              Progress is computed live from individual reprice rows (up to
              1,000 fetched per batch).
            </p>
          </CardContent>
        </Card>
      )}
    </div>
  )
}

function CancelMigrationButton({
  repriceIds,
  onDone,
}: {
  repriceIds: string[]
  onDone: () => void
}) {
  const [busy, setBusy] = React.useState(false)
  return (
    <Button
      variant="destructive"
      size="sm"
      disabled={busy || !repriceIds.length}
      onClick={async () => {
        setBusy(true)
        try {
          await Promise.all(repriceIds.map((id) => cancelReprice(id)))
          toast.success(
            `Canceled ${repriceIds.length} pending reprice${repriceIds.length === 1 ? "" : "s"}. Already-migrated subscribers stay migrated.`
          )
          onDone()
        } catch (err) {
          toastApiError(err, "Cancel migration")
        } finally {
          setBusy(false)
        }
      }}
    >
      {busy ? "Canceling…" : "Cancel migration"}
    </Button>
  )
}
