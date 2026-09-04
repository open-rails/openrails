import { HugeiconsIcon } from "@hugeicons/react"
import {
  Activity01Icon,
  ArrowLeft01Icon,
  ArrowRight01Icon,
  Delete02Icon,
  InformationCircleIcon,
  Search01Icon,
} from "@hugeicons/core-free-icons"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import * as React from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { toast } from "sonner"

import { PaginationFooter } from "@/components/pagination-footer"
import { LinkedTableRow } from "@/components/linked-table-row"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
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
import { Input } from "@/components/ui/input"
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
import type { UsageMeter } from "@/lib/api/types"
import { formatDate } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"
import { toastApiError } from "@/lib/toast"
import { MeterFormDialog } from "./meter-form"
import {
  meterCollectionState,
  meterDefinitionLocked,
  summarizeRateCard,
} from "./metering-model"
import { RateCardEditor } from "./rate-card-editor"

const PAGE_SIZE = 200

export function CatalogMeteringPage() {
  const [search, setSearch] = React.useState("")
  const [offset, setOffset] = React.useState(0)
  const { data, isPending, error, refetch } = useQuery(
    adminQueries.usageMeters(PAGE_SIZE, offset)
  )
  const meters = data?.items ?? []
  const normalizedSearch = search.trim().toLowerCase()
  const filtered = normalizedSearch
    ? meters.filter((meter) =>
        [
          meter.key,
          meter.effective_event_type,
          meter.aggregation,
          meter.unit,
          meter.default_rate_card?.product_key,
        ].some((value) => value?.toLowerCase().includes(normalizedSearch))
      )
    : meters
  const state = meterCollectionState({
    pending: isPending,
    error: error instanceof ApiError ? error : error ? { status: 500 } : null,
    count: meters.length,
  })
  const writesAllowed = data?.writes_allowed ?? false

  return (
    <div className="flex min-w-0 flex-col gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="max-w-2xl">
          <h1 className="text-2xl font-semibold tracking-tight text-balance">
            Metering
          </h1>
          <p className="mt-1 text-sm text-pretty text-muted-foreground">
            Define usage streams and the default rates used for in-arrears
            billing. Applications report the events separately.
          </p>
        </div>
        {writesAllowed && state !== "permission" && <MeterFormDialog />}
      </div>

      {data && !writesAllowed && (
        <ReadOnlyNotice>
          This catalog is manifest-managed. Meter definitions and rates are
          visible here but must be changed in the host catalog manifest.
        </ReadOnlyNotice>
      )}

      {state === "loading" ? (
        <MeteringSkeleton />
      ) : state === "permission" ? (
        <PageMessage
          title="Metering access is restricted"
          description="Your role does not have permission to read the merchant catalog."
        />
      ) : state === "error" ? (
        <PageMessage
          title="Metering could not be loaded"
          description="Try again. If the problem continues, check the OpenRails API logs."
          action={
            <Button variant="outline" size="sm" onClick={() => void refetch()}>
              Try again
            </Button>
          }
        />
      ) : state === "empty" ? (
        <PageMessage
          title="No usage meters yet"
          description="Create a meter to describe the events your application will report and bill."
          action={writesAllowed ? <MeterFormDialog /> : undefined}
        />
      ) : (
        <>
          <div className="relative w-full sm:max-w-xs">
            <HugeiconsIcon
              icon={Search01Icon}
              className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground"
            />
            <Input
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              className="pl-8"
              placeholder="Filter this page…"
              aria-label="Filter meters on this page"
            />
          </div>
          <div className="max-w-full overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="text-muted-foreground">Meter</TableHead>
                  <TableHead className="text-muted-foreground">Event</TableHead>
                  <TableHead className="text-muted-foreground">
                    Aggregation
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    Default price
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    Product
                  </TableHead>
                  <TableHead className="text-right text-muted-foreground">
                    Overrides
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    Activity
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    Last event
                  </TableHead>
                  <TableHead>
                    <span className="sr-only">Open</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filtered.map((meter) => (
                  <MeterRow key={meter.key} meter={meter} />
                ))}
                {filtered.length === 0 && (
                  <TableRow>
                    <TableCell
                      colSpan={9}
                      className="h-24 text-center text-muted-foreground"
                    >
                      No meters on this page match “{search}”.
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </div>
          <PaginationFooter
            total={data?.total ?? 0}
            limit={data?.limit ?? PAGE_SIZE}
            offset={data?.offset ?? offset}
            loading={isPending}
            onChange={setOffset}
          />
        </>
      )}
    </div>
  )
}

function MeterRow({ meter }: { meter: UsageMeter }) {
  return (
    <LinkedTableRow to={`/catalog/metering/${encodeURIComponent(meter.key)}`}>
      <TableCell>
        <Link
          className="font-medium underline-offset-3 hover:underline"
          to={`/catalog/metering/${encodeURIComponent(meter.key)}`}
        >
          {meter.key}
        </Link>
        {meter.unit && (
          <p className="text-xs text-muted-foreground">{meter.unit}</p>
        )}
      </TableCell>
      <TableCell>{meter.effective_event_type}</TableCell>
      <TableCell>
        <Badge variant="secondary">{meter.aggregation}</Badge>
      </TableCell>
      <TableCell className="font-medium tabular-nums">
        {summarizeRateCard(meter.default_rate_card)}
      </TableCell>
      <TableCell>{meter.default_rate_card?.product_key ?? "—"}</TableCell>
      <TableCell className="text-right tabular-nums">
        {meter.override_count.toLocaleString()}
      </TableCell>
      <TableCell>
        <span className="inline-flex items-center gap-1.5 text-sm">
          <span
            className={`size-1.5 rounded-full ${meter.has_activity ? "bg-settled" : "bg-muted-foreground/50"}`}
          />
          {meter.has_activity ? "Active" : "No events"}
        </span>
      </TableCell>
      <TableCell className="text-muted-foreground tabular-nums">
        {formatDate(meter.last_event_at)}
      </TableCell>
      <TableCell className="text-right">
        <HugeiconsIcon
          icon={ArrowRight01Icon}
          className="ml-auto size-4 text-muted-foreground"
        />
      </TableCell>
    </LinkedTableRow>
  )
}

export function MeterDetailPage() {
  const { key = "" } = useParams()
  const meterKey = key
  const navigate = useNavigate()
  const [overrideOffset, setOverrideOffset] = React.useState(0)
  const meterQuery = useQuery(adminQueries.usageMeter(meterKey))
  const metersQuery = useQuery(adminQueries.allUsageMeters())
  const productsQuery = useQuery(
    adminQueries.allProducts({ errorAction: "Load products for metering" })
  )
  const overridesQuery = useQuery(
    adminQueries.usageMeterOverrides(meterKey, PAGE_SIZE, overrideOffset)
  )

  if (meterQuery.isPending) return <MeterDetailSkeleton />
  if (meterQuery.error) {
    const notFound =
      meterQuery.error instanceof ApiError && meterQuery.error.status === 404
    const forbidden =
      meterQuery.error instanceof ApiError && meterQuery.error.status === 403
    return (
      <PageMessage
        title={
          forbidden
            ? "Metering access is restricted"
            : notFound
              ? "Meter not found"
              : "Meter could not be loaded"
        }
        description={
          forbidden
            ? "Your role does not have permission to read this meter."
            : notFound
              ? "This meter does not exist in the active merchant catalog."
              : "Try loading the meter again."
        }
        action={
          forbidden ? undefined : (
            <Button
              variant="outline"
              size="sm"
              onClick={() =>
                notFound
                  ? navigate("/catalog/metering")
                  : void meterQuery.refetch()
              }
            >
              {notFound ? "Back to metering" : "Try again"}
            </Button>
          )
        }
      />
    )
  }
  const meter = meterQuery.data
  if (!meter) return null
  const definitionLock = meterDefinitionLocked(meter)

  return (
    <div className="flex min-w-0 flex-col gap-8">
      <header className="flex flex-wrap items-start gap-3">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => navigate("/catalog/metering")}
          aria-label="Back to metering"
        >
          <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
        </Button>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="truncate text-xl font-semibold tracking-tight">
              {meter.key}
            </h1>
            {meter.configuration_source === "manifest" && (
              <Badge variant="secondary">manifest</Badge>
            )}
          </div>
          <p className="mt-1 text-sm text-muted-foreground">
            {meter.effective_event_type} · {meter.aggregation}
            {meter.unit ? ` · ${meter.unit}` : ""}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <MeterFormDialog meter={meter} />
          <RateCardEditor
            meter={meter}
            products={productsQuery.data?.items ?? []}
            meters={metersQuery.data?.items ?? []}
            productsPending={productsQuery.isPending}
            productsError={Boolean(productsQuery.error)}
            productsForbidden={
              productsQuery.error instanceof ApiError &&
              productsQuery.error.status === 403
            }
          />
        </div>
      </header>

      {!meter.writes_allowed && (
        <ReadOnlyNotice>
          This meter is managed by the catalog manifest. The console is
          read-only.
        </ReadOnlyNotice>
      )}

      <section className="grid gap-4">
        <div>
          <h2 className="text-base font-semibold">Event contract</h2>
          <p className="mt-1 max-w-3xl text-sm text-pretty text-muted-foreground">
            Creating this meter does not send events. The host application must
            report idempotent usage to{" "}
            <code className="text-foreground">/merchant/usage/report</code> with
            this shape.
          </p>
        </div>
        <dl className="grid gap-x-8 gap-y-4 border-y py-4 sm:grid-cols-2 lg:grid-cols-4">
          <ContractFact label="Event type" value={meter.effective_event_type} />
          <ContractFact
            label="Aggregation"
            value={
              meter.aggregation === "sum"
                ? `Sum ${meter.value_property}`
                : "Count each event"
            }
          />
          <ContractFact label="Meter key" value={meter.key} />
          <ContractFact
            label="Billing support"
            value={meter.billing_supported ? "Supported" : "Read only"}
          />
          {Object.entries(meter.group_by).map(([name, property]) => (
            <ContractFact
              key={name}
              label={`Dimension: ${name}`}
              value={property}
            />
          ))}
        </dl>
        {definitionLock && meter.writes_allowed && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <HugeiconsIcon icon={Activity01Icon} className="size-4" />
            {definitionLock}
          </p>
        )}
      </section>

      <DefaultRateSection meter={meter} />

      <section className="grid gap-3">
        <div>
          <h2 className="text-base font-semibold">Negotiated overrides</h2>
          <p className="mt-1 text-sm text-muted-foreground">
            Customer-specific prices replacing this default rate.
          </p>
        </div>
        {overridesQuery.isPending ? (
          <div className="grid gap-2">
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
          </div>
        ) : overridesQuery.error ? (
          <PageMessage
            title={
              overridesQuery.error instanceof ApiError &&
              overridesQuery.error.status === 403
                ? "Override access is restricted"
                : "Overrides could not be loaded"
            }
            description={
              overridesQuery.error instanceof ApiError &&
              overridesQuery.error.status === 403
                ? "Your role does not have permission to read negotiated rates."
                : "Try loading this list again."
            }
            action={
              overridesQuery.error instanceof ApiError &&
              overridesQuery.error.status === 403 ? undefined : (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => void overridesQuery.refetch()}
                >
                  Try again
                </Button>
              )
            }
            compact
          />
        ) : (overridesQuery.data?.items.length ?? 0) === 0 ? (
          <p className="border-y py-6 text-sm text-muted-foreground">
            No customers have a negotiated rate for this meter.
          </p>
        ) : (
          <>
            <div className="max-w-full overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead className="text-muted-foreground">
                      Customer
                    </TableHead>
                    <TableHead className="text-muted-foreground">
                      Rate
                    </TableHead>
                    <TableHead className="text-muted-foreground">
                      Allowance
                    </TableHead>
                    <TableHead className="text-muted-foreground">
                      Updated
                    </TableHead>
                    <TableHead />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {overridesQuery.data?.items.map((override) => (
                    <LinkedTableRow
                      key={override.customer_id}
                      to={`/customers/${override.customer_id}`}
                    >
                      <TableCell>
                        <Link
                          className="font-medium underline-offset-3 hover:underline"
                          to={`/customers/${override.customer_id}`}
                        >
                          {override.email ||
                            override.subject ||
                            override.customer_id}
                        </Link>
                        {(override.email || override.subject) && (
                          <p className="text-xs text-muted-foreground">
                            {override.customer_id}
                          </p>
                        )}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {summarizeRateCard({
                          price: override.price,
                        } as NonNullable<UsageMeter["default_rate_card"]>)}
                      </TableCell>
                      <TableCell>
                        {override.allowance?.included !== undefined
                          ? `${override.allowance.included.toLocaleString()} included`
                          : override.allowance?.accrue_from
                            ? `From ${override.allowance.accrue_from}`
                            : "—"}
                      </TableCell>
                      <TableCell className="text-muted-foreground tabular-nums">
                        {formatDate(override.updated_at)}
                      </TableCell>
                      <TableCell>
                        <HugeiconsIcon
                          icon={ArrowRight01Icon}
                          className="ml-auto size-4 text-muted-foreground"
                        />
                      </TableCell>
                    </LinkedTableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
            <PaginationFooter
              total={overridesQuery.data?.total ?? 0}
              limit={overridesQuery.data?.limit ?? PAGE_SIZE}
              offset={overridesQuery.data?.offset ?? overrideOffset}
              loading={overridesQuery.isPending}
              onChange={setOverrideOffset}
            />
          </>
        )}
      </section>
    </div>
  )
}

function DefaultRateSection({ meter }: { meter: UsageMeter }) {
  const [deleteOpen, setDeleteOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const remove = useMutation(
    adminMutations.deleteDefaultUsageRateCard(queryClient)
  )
  const card = meter.default_rate_card
  const blocked = meter.override_count > 0
  const handleDelete = async () => {
    try {
      await remove.mutateAsync(meter.key)
      toast.success("Default rate removed")
      setDeleteOpen(false)
    } catch (error) {
      toastApiError(error, "Remove default rate")
    }
  }
  return (
    <section className="grid gap-4 border-t pt-7">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h2 className="text-base font-semibold">Default rate</h2>
          <p className="mt-1 text-sm text-muted-foreground">
            Used for every customer without a negotiated override.
          </p>
        </div>
        {card && meter.writes_allowed && (
          <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
            <AlertDialogTrigger
              render={
                <Button
                  variant="outline"
                  size="sm"
                  disabled={blocked}
                  title={
                    blocked
                      ? "Remove every negotiated override first."
                      : undefined
                  }
                >
                  <HugeiconsIcon icon={Delete02Icon} className="size-4" />
                  Remove default
                </Button>
              }
            />
            <AlertDialogContent>
              <AlertDialogHeader>
                <AlertDialogTitle>Remove the default rate?</AlertDialogTitle>
                <AlertDialogDescription>
                  New usage for this meter will stop being rated after removal.
                  Historical invoices and accrued amounts remain unchanged.
                </AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel>Keep rate</AlertDialogCancel>
                <AlertDialogAction
                  variant="destructive"
                  disabled={remove.isPending}
                  onClick={() => void handleDelete()}
                >
                  {remove.isPending ? "Removing…" : "Remove default"}
                </AlertDialogAction>
              </AlertDialogFooter>
            </AlertDialogContent>
          </AlertDialog>
        )}
      </div>
      {!meter.billing_supported ? (
        <ReadOnlyNotice>
          This meter uses an aggregation that the billing engine cannot rate.
        </ReadOnlyNotice>
      ) : !card ? (
        <p className="border-y py-6 text-sm text-muted-foreground">
          No default rate. Usage is recorded but is not billed by this meter.
        </p>
      ) : (
        <dl className="grid gap-x-8 gap-y-4 border-y py-4 sm:grid-cols-2 lg:grid-cols-4">
          <ContractFact label="Price" value={summarizeRateCard(card)} />
          <ContractFact label="Product" value={card.product_key} />
          <ContractFact
            label="Model"
            value={card.price.model.replace("_", " ")}
          />
          <ContractFact
            label="Overrides"
            value={meter.override_count.toLocaleString()}
          />
          <ContractFact
            label="Filter"
            value={
              Object.keys(card.filter).length
                ? Object.entries(card.filter)
                    .map(([key, values]) => `${key}: ${values.join(", ")}`)
                    .join(" · ")
                : "All matching events"
            }
          />
          <ContractFact
            label="Allowance"
            value={
              card.allowance?.included !== undefined
                ? `${card.allowance.included.toLocaleString()} included`
                : card.allowance?.accrue_from
                  ? `Accrues from ${card.allowance.accrue_from}, capped at ${card.allowance.cap}`
                  : "None"
            }
          />
          <ContractFact label="Updated" value={formatDate(card.updated_at)} />
        </dl>
      )}
      {blocked && card && meter.writes_allowed && (
        <p className="text-xs text-muted-foreground">
          Remove all {meter.override_count.toLocaleString()} negotiated override
          {meter.override_count === 1 ? "" : "s"} below before deleting the
          default rate.
        </p>
      )}
    </section>
  )
}

function ContractFact({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="mt-1 text-sm font-medium break-words tabular-nums">
        {value || "—"}
      </dd>
    </div>
  )
}

function ReadOnlyNotice({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex items-start gap-2 rounded-lg bg-muted/50 px-3 py-2.5 text-sm text-muted-foreground">
      <HugeiconsIcon
        icon={InformationCircleIcon}
        className="mt-0.5 size-4 shrink-0"
      />
      <p>{children}</p>
    </div>
  )
}

function PageMessage({
  title,
  description,
  action,
  compact = false,
}: {
  title: string
  description: string
  action?: React.ReactNode
  compact?: boolean
}) {
  return (
    <div
      className={`flex flex-col items-start border-y ${compact ? "gap-2 py-5" : "gap-3 py-10"}`}
    >
      <div>
        <h2 className="text-sm font-medium">{title}</h2>
        <p className="mt-1 text-sm text-muted-foreground">{description}</p>
      </div>
      {action}
    </div>
  )
}

function MeteringSkeleton() {
  return (
    <div className="grid gap-3">
      <Skeleton className="h-8 w-64" />
      {Array.from({ length: 5 }).map((_, index) => (
        <Skeleton key={index} className="h-11 w-full" />
      ))}
    </div>
  )
}

function MeterDetailSkeleton() {
  return (
    <div className="grid gap-6">
      <Skeleton className="h-10 w-72" />
      <Skeleton className="h-28 w-full" />
      <Skeleton className="h-36 w-full" />
      <Skeleton className="h-40 w-full" />
    </div>
  )
}
