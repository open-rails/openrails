import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Link, useParams, useSearchParams } from "#orck/admin/router"

import { CursorPager } from "#orck/admin/components/cursor-pager"
import { Button } from "#orck/admin/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "#orck/admin/components/ui/dialog"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import {
  useAdminArea,
  useCatalogArea,
  useMerchantConfig,
  useMerchantConfigUpdates,
} from "#orck/admin/lib/capabilities"
import { useUrlCursor } from "#orck/admin/lib/cursor-pages"
import { DIALOG_FORM } from "#orck/admin/lib/dialog-width"
import { formatNativeAmount, shortId } from "#orck/admin/lib/format"
import { importBusy } from "#orck/admin/lib/import-queries"
import type {
  Customer,
  LinkPSPImportParams,
  PSPImport,
  PSPImportRecord,
} from "#orck/client/generated/wire"
import { pspKey, selectClass, usePSPs } from "./shared"

const STATES: PSPImportRecord["state"][] = [
  "unmatched",
  "conflict",
  "blocked",
  "linked",
  "imported",
]
const KINDS: PSPImportRecord["kind"][] = [
  "customer",
  "subscription",
  "transaction",
]
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export function ImportRecordsPage() {
  return useMerchantConfig() ? <Records /> : null
}

function Records() {
  const { text, id: adminId } = useAdminUI()
  const { id = "" } = useParams<"id">()
  const { importQueries } = useAdminServices()
  const canUpdate = useMerchantConfigUpdates()
  const adminRead = useAdminArea()
  const psps = usePSPs()
  const [params, setParams] = useSearchParams()
  const state = (params.get("state") ?? "unmatched") as PSPImportRecord["state"]
  const kind = (params.get("kind") || undefined) as
    PSPImportRecord["kind"] | undefined
  const paging = useUrlCursor(50)
  const imp = useQuery(importQueries.detail(id)).data
  const { data, isPending, isFetching } = useQuery(
    importQueries.records(
      id,
      { state, kind },
      paging.size,
      paging.cursor || undefined
    )
  )
  const ids = [
    ...new Set(data?.data.flatMap((r) => (r.customer ? [r.customer] : []))),
  ]
  const customers = useQuery({
    ...importQueries.customers(ids),
    enabled: adminRead && ids.length > 0,
  }).data
  const emails = new Map(
    customers?.data.map((c) => [c.id, c.contact?.email ?? null])
  )
  const [linking, setLinking] = React.useState<PSPImportRecord>()
  const [pricing, setPricing] = React.useState<PSPImportRecord>()
  const set = (key: string, value: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    next.delete("cursor")
    setParams(next)
  }
  const editable = canUpdate && imp && !importBusy(imp.status)
  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-xl font-semibold">
          <AdminMessage id="Import records" />
        </h1>
        <p className="text-sm text-muted-foreground">
          {imp && (
            <Link className="underline" to={`/ops/imports/${imp.id}`}>
              {text("Import of ")}
              {pspKey(psps.byId, imp.psp)}
            </Link>
          )}
        </p>
      </div>
      <div className="flex flex-wrap items-end gap-3">
        <nav className="flex gap-1" aria-label={text("Record state")}>
          {STATES.map((s) => (
            <Button
              key={s}
              size="sm"
              variant={s === state ? "secondary" : "ghost"}
              aria-pressed={s === state}
              onClick={() => set("state", s)}
            >
              {text(s)}
            </Button>
          ))}
        </nav>
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("records-kind")}>
            <AdminMessage id="Kind" />
          </Label>
          <select
            id={adminId("records-kind")}
            className={selectClass}
            value={kind ?? ""}
            onChange={(e) => set("kind", e.target.value)}
          >
            <option value="">{text("All kinds")}</option>
            {KINDS.map((k) => (
              <option key={k} value={k}>
                {text(k)}
              </option>
            ))}
          </select>
        </div>
      </div>
      <div className="overflow-x-auto rounded-md border">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              {["Kind", "Provider ref", "Facts", "Customer", "Why", ""].map(
                (h, i) => (
                  <TableHead key={i} className="text-muted-foreground">
                    {h && text(h)}
                  </TableHead>
                )
              )}
            </TableRow>
          </TableHeader>
          <TableBody>
            {isPending ? (
              <TableRow>
                <TableCell colSpan={6}>
                  <AdminMessage id="Loading…" />
                </TableCell>
              </TableRow>
            ) : !data?.data.length ? (
              <TableRow>
                <TableCell colSpan={6} className="text-muted-foreground">
                  <AdminMessage id="No records in this state." />
                </TableCell>
              </TableRow>
            ) : (
              data.data.map((r) => (
                <TableRow key={r.id} data-ref={r.ref}>
                  <TableCell>{text(r.kind)}</TableCell>
                  <TableCell className="font-mono text-xs">{r.ref}</TableCell>
                  <TableCell className="text-xs">
                    <Facts record={r} />
                  </TableCell>
                  <TableCell className="text-xs">
                    {r.customer ? (
                      <>
                        <span className="font-mono">{r.customer}</span>
                        {emails.get(r.customer) && (
                          <span className="block">
                            {emails.get(r.customer)}
                          </span>
                        )}
                      </>
                    ) : (
                      "—"
                    )}
                  </TableCell>
                  <TableCell className="max-w-72 text-xs">
                    {r.linked_via && (
                      <span className="block">
                        {text("via ")}
                        {r.linked_via}
                      </span>
                    )}
                    {r.manual_price && (
                      <span className="block">
                        {text("price ")}
                        {shortId(r.manual_price, 14)}
                      </span>
                    )}
                    {r.reason}
                  </TableCell>
                  <TableCell className="space-x-1 text-right whitespace-nowrap">
                    {editable && r.state !== "imported" && imp && (
                      <RecordActions
                        imp={imp}
                        record={r}
                        onLink={() => setLinking(r)}
                        onPrice={() => setPricing(r)}
                      />
                    )}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>
      <CursorPager
        pages={paging}
        rows={data?.data.length ?? 0}
        nextCursor={data?.next_cursor}
        busy={isFetching}
      />
      {linking && imp && (
        <LinkDialog
          imp={imp}
          record={linking}
          onClose={() => setLinking(undefined)}
        />
      )}
      {pricing && imp && (
        <PriceDialog
          imp={imp}
          record={pricing}
          onClose={() => setPricing(undefined)}
        />
      )}
    </div>
  )
}

function Facts({ record }: { record: PSPImportRecord }) {
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-2">
      {record.facts.map((f) => (
        <React.Fragment key={f.name}>
          <dt className="text-muted-foreground">{f.name}</dt>
          <dd className="break-all">{f.value}</dd>
        </React.Fragment>
      ))}
    </dl>
  )
}

function useLink(imp: PSPImport) {
  const { importMutations, toast, toastApiError } = useAdminServices()
  const { text } = useAdminUI()
  const queryClient = useQueryClient()
  const link = useMutation(importMutations.link(queryClient))
  return {
    pending: link.isPending,
    send: async (params: LinkPSPImportParams, done: string) => {
      try {
        await link.mutateAsync({ id: imp.id, params })
        toast.success(done)
        return true
      } catch (err) {
        toastApiError(err, text("Link record"))
        return false
      }
    },
  }
}

function RecordActions({
  imp,
  record,
  onLink,
  onPrice,
}: {
  imp: PSPImport
  record: PSPImportRecord
  onLink: () => void
  onPrice: () => void
}) {
  const { text } = useAdminUI()
  const link = useLink(imp)
  return (
    <>
      <Button size="sm" variant="outline" onClick={onLink}>
        <AdminMessage id="Link to customer" />
      </Button>
      {record.linked_via === "manual" && (
        <Button
          size="sm"
          variant="ghost"
          disabled={link.pending}
          onClick={() =>
            void link.send(
              { links: [{ record: record.id, customer: null }] },
              text("Link removed")
            )
          }
        >
          <AdminMessage id="Unlink" />
        </Button>
      )}
      {imp.rail === "ccbill" && record.kind === "subscription" && (
        <Button size="sm" variant="outline" onClick={onPrice}>
          <AdminMessage id="Set price" />
        </Button>
      )}
    </>
  )
}

// LinkDialog links a record to a customer by hand; a manual link wins over
// every rule until it is removed.
function LinkDialog({
  imp,
  record,
  onClose,
}: {
  imp: PSPImport
  record: PSPImportRecord
  onClose: () => void
}) {
  const { text, id: adminId } = useAdminUI()
  const { importQueries, adminQueries } = useAdminServices()
  const adminRead = useAdminArea()
  const link = useLink(imp)
  const [customer, setCustomer] = React.useState(record.customer ?? "")
  const [search, setSearch] = React.useState("")
  const valid = UUID.test(customer.trim())
  const named = useQuery({
    ...importQueries.customers(valid ? [customer.trim().toLowerCase()] : []),
    enabled: adminRead && valid,
  }).data?.data[0]
  const found = useQuery({
    ...adminQueries.customers(search.trim(), 10, ""),
    enabled: adminRead && search.trim().length > 2,
  }).data?.data
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id="Link to customer" />
          </DialogTitle>
          <DialogDescription>
            {record.kind} {record.ref}
          </DialogDescription>
        </DialogHeader>
        <div className="rounded-md bg-muted p-3 text-xs">
          <Facts record={record} />
          {record.reason && <p className="mt-2">{record.reason}</p>}
        </div>
        {adminRead && (
          <div className="grid gap-1.5">
            <Label htmlFor={adminId("link-search")}>
              <AdminMessage id="Find a customer with billing" />
            </Label>
            <Input
              id={adminId("link-search")}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
            {found?.map((c: Customer) => (
              <button
                key={c.id}
                type="button"
                className="text-left text-sm underline"
                onClick={() => setCustomer(c.id)}
              >
                {c.contact?.email ?? c.id}
              </button>
            ))}
          </div>
        )}
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("link-customer")}>
            <AdminMessage id="Customer id" />
          </Label>
          <Input
            id={adminId("link-customer")}
            value={customer}
            onChange={(e) => setCustomer(e.target.value)}
            aria-invalid={customer !== "" && !valid}
          />
          {named?.contact?.email && (
            <p className="text-sm" data-testid="link-email">
              {named.contact.email}
            </p>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            <AdminMessage id="Cancel" />
          </Button>
          <Button
            disabled={!valid || link.pending}
            onClick={async () => {
              const ok = await link.send(
                {
                  links: [
                    {
                      record: record.id,
                      customer: customer.trim().toLowerCase(),
                    },
                  ],
                },
                text("Record linked")
              )
              if (ok) onClose()
            }}
          >
            <AdminMessage id="Link" />
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// PriceDialog picks the provider price a CCBill subscription lands on when
// its rebill matches no single price: one bound to the import's PSP.
function PriceDialog({
  imp,
  record,
  onClose,
}: {
  imp: PSPImport
  record: PSPImportRecord
  onClose: () => void
}) {
  const { text, id: adminId } = useAdminUI()
  const { adminQueries } = useAdminServices()
  const catalog = useCatalogArea()
  const link = useLink(imp)
  const prices = useQuery({
    ...adminQueries.allPrices(),
    enabled: catalog,
  }).data?.data.filter(
    (p) =>
      p.provider?.psp_id === imp.psp &&
      (p.billing_interval_hours || p.billing_interval_months)
  )
  const [price, setPrice] = React.useState(record.manual_price ?? "")
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id="Set price" />
          </DialogTitle>
          <DialogDescription>
            {record.ref}
            {record.reason ? ` · ${record.reason}` : ""}
          </DialogDescription>
        </DialogHeader>
        {!catalog ? (
          <p className="text-sm text-muted-foreground">
            <AdminMessage id="Listing prices needs catalog read." />
          </p>
        ) : (
          <div className="grid gap-1.5">
            <Label htmlFor={adminId("record-price")}>
              <AdminMessage id="Provider price" />
            </Label>
            <select
              id={adminId("record-price")}
              className={selectClass}
              value={price}
              onChange={(e) => setPrice(e.target.value)}
            >
              <option value="">{text("Choose a price")}</option>
              {prices?.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.key} · {formatNativeAmount(p.unit_amount, p.currency)}{" "}
                  {p.currency}
                  {p.archived ? ` (${text("archived")})` : ""}
                </option>
              ))}
            </select>
          </div>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            <AdminMessage id="Cancel" />
          </Button>
          {record.manual_price && (
            <Button
              variant="ghost"
              disabled={link.pending}
              onClick={async () => {
                const ok = await link.send(
                  { prices: [{ record: record.id, price: null }] },
                  text("Price cleared")
                )
                if (ok) onClose()
              }}
            >
              <AdminMessage id="Clear" />
            </Button>
          )}
          <Button
            disabled={!price || link.pending}
            onClick={async () => {
              const ok = await link.send(
                { prices: [{ record: record.id, price }] },
                text("Price set")
              )
              if (ok) onClose()
            }}
          >
            <AdminMessage id="Set price" />
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
