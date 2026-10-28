import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Link, useParams } from "#orck/admin/router"

import { StatusBadge } from "#orck/admin/components/status-badge"
import { TypedConfirmDialog } from "#orck/admin/components/typed-confirm-dialog"
import { Badge } from "#orck/admin/components/ui/badge"
import { Button } from "#orck/admin/components/ui/button"
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "#orck/admin/components/ui/card"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import {
  useMerchantConfig,
  useMerchantConfigUpdates,
} from "#orck/admin/lib/capabilities"
import { importBusy } from "#orck/admin/lib/import-queries"
import { formatDate, shortId } from "#orck/admin/lib/format"
import type { PSP, PSPImport } from "#orck/client/generated/wire"
import {
  MapButton,
  MapPriceDialog,
} from "#orck/admin/pages/provider-prices/map-dialog"
import { importProgress, recordCount } from "./progress"
import { ProgressText, ruleText, termsText, usePSPs } from "./shared"

const KINDS = ["customer", "subscription", "transaction"] as const
const STATES = [
  "unmatched",
  "conflict",
  "linked",
  "blocked",
  "held",
  "imported",
] as const

export function ImportDetailPage() {
  return useMerchantConfig() ? <ImportDetail /> : null
}

function ImportDetail() {
  const { text } = useAdminUI()
  const { id = "" } = useParams<"id">()
  const { importQueries } = useAdminServices()
  const psps = usePSPs()
  const { data: imp, error } = useQuery(importQueries.detail(id))
  if (error)
    return (
      <p role="alert" className="text-destructive">
        {error.message}
      </p>
    )
  if (!imp)
    return (
      <p role="status">
        <AdminMessage id="Loading…" />
      </p>
    )
  const psp = psps.byId.get(imp.psp)
  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <h1 className="flex items-center gap-2 text-xl font-semibold">
            {text("Import of ")}
            {psp?.key ?? imp.psp}
            <Badge variant="secondary">{imp.origin}</Badge>
            <StatusBadge status={imp.status} />
          </h1>
          <p className="text-sm text-muted-foreground">
            {text("Started ")}
            {formatDate(imp.created_at)}
            {imp.applied_at && (
              <>
                {text(" · applied ")}
                {formatDate(imp.applied_at)}
              </>
            )}
          </p>
        </div>
        <ImportActions imp={imp} psp={psp} />
      </div>
      <Progress imp={imp} />
      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            <AdminMessage id="Records" />
          </CardTitle>
        </CardHeader>
        <CardContent className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead />
                {STATES.map((s) => (
                  <TableHead key={s} className="text-muted-foreground">
                    {text(s)}
                  </TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {KINDS.map((kind) => (
                <TableRow key={kind} data-kind={kind}>
                  <TableCell className="font-medium">{text(kind)}</TableCell>
                  {STATES.map((state) => {
                    const n = recordCount(imp, state, kind)
                    return (
                      <TableCell key={state} data-state={state}>
                        {n ? (
                          <Link
                            className="underline"
                            to={`/ops/imports/${imp.id}/records?state=${state}&kind=${kind}`}
                          >
                            {n}
                          </Link>
                        ) : (
                          "0"
                        )}
                      </TableCell>
                    )
                  })}
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <Link
            className="mt-3 inline-block text-sm underline"
            to={`/ops/imports/${imp.id}/records`}
          >
            <AdminMessage id="Review records" />
          </Link>
        </CardContent>
      </Card>
      <ProposalCounts imp={imp} />
      {psp && <Plans imp={imp} psp={psp} />}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            <AdminMessage id="Linking rules" />
          </CardTitle>
        </CardHeader>
        <CardContent>
          <ol className="list-decimal pl-5 text-sm">
            {imp.rules.map((rule, i) => (
              <li key={i}>{ruleText(rule)}</li>
            ))}
          </ol>
          <p className="mt-2 text-xs text-muted-foreground">
            <AdminMessage id="A manual link wins over every rule." />
          </p>
        </CardContent>
      </Card>
    </div>
  )
}

function Progress({ imp }: { imp: PSPImport }) {
  const p = importProgress(imp)
  if (imp.status === "failed")
    return (
      <div
        role="alert"
        className="rounded-md border border-destructive/40 p-3 text-sm"
      >
        <p className="font-medium text-destructive">
          {imp.error_code ?? "failed"}: {imp.error}
        </p>
        {imp.error_code === "psp_credentials_rejected" && (
          <p className="mt-1">
            <AdminMessage id="The provider refused this PSP's credentials. A new pull starts when they change in " />
            <Link className="underline" to="/settings?tab=psps">
              <AdminMessage id="Settings › PSPs" />
            </Link>
            .
          </p>
        )}
        {imp.error_code === "pull_roster_implausible" && (
          <p className="mt-1">
            <AdminMessage id="The provider listed far fewer live subscriptions than OpenRails holds, so nothing was proposed. Check the PSP's account and reconcile again." />
          </p>
        )}
        {imp.error_code === "psp_not_readable" && (
          <p className="mt-1">
            <AdminMessage id="This PSP cannot be read: it is gone, or a credential the pull needs is not declared." />
          </p>
        )}
      </div>
    )
  return (
    <div className="flex flex-col gap-1" data-testid="import-progress">
      <p className="text-sm">
        <ProgressText imp={imp} />
      </p>
      {p.phase === "history" && p.percent !== undefined && (
        <div className="h-1.5 w-full max-w-md rounded bg-muted">
          <div
            className="h-1.5 rounded bg-primary"
            style={{ width: `${p.percent}%` }}
          />
        </div>
      )}
    </div>
  )
}

// ProposalCounts are the differences the pull found in what OpenRails holds:
// Apply approves the corrections; the rest wait in the inbox.
function ProposalCounts({ imp }: { imp: PSPImport }) {
  const { text } = useAdminUI()
  const p = imp.proposals
  if (!p.open && !p.approved && !p.dismissed && !p.superseded) return null
  return (
    <p className="text-sm" data-testid="import-proposals">
      <Link className="underline" to={`/ops/proposals?pull_id=${imp.id}`}>
        {p.open}
        {text(" open proposals")}
      </Link>
      {` (${p.corrections}${text(" corrections Apply approves")}) · ${p.approved}${text(" approved")} · ${p.dismissed}${text(" dismissed")} · ${p.superseded}${text(" superseded")}`}
    </p>
  )
}

// Plans are the provider prices the import's subscriptions name: each must
// map to a product before its subscriptions land.
function Plans({ imp, psp }: { imp: PSPImport; psp: PSP }) {
  const { text } = useAdminUI()
  const { importQueries } = useAdminServices()
  const canUpdate = useMerchantConfigUpdates()
  const [mapping, setMapping] = React.useState<string>()
  const observed = psp.rail === "nmi" || psp.rail === "stripe"
  const { data } = useQuery({
    ...importQueries.providerCatalog(
      { psp: psp.id, kind: "price", import: imp.id },
      500
    ),
    enabled: observed && Boolean(imp.pulled_at),
  })
  if (!observed) return null
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">
          <AdminMessage id="Plans its subscriptions name" />
        </CardTitle>
      </CardHeader>
      <CardContent className="overflow-x-auto">
        {!data?.data.length ? (
          <p className="text-sm text-muted-foreground">
            <AdminMessage id="No staged subscription names a plan." />
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                {[
                  "Plan",
                  "Name",
                  "Terms",
                  "Subscriptions",
                  "Mapped to",
                  "",
                ].map((h, i) => (
                  <TableHead key={i} className="text-muted-foreground">
                    {h && text(h)}
                  </TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.data.map((o) => (
                <TableRow key={o.ref} data-ref={o.ref}>
                  <TableCell className="font-mono text-xs">{o.ref}</TableCell>
                  <TableCell>{o.name ?? "—"}</TableCell>
                  <TableCell>{termsText(o.terms, psp.rail)}</TableCell>
                  <TableCell>{o.subscriptions ?? "—"}</TableCell>
                  <TableCell>
                    {o.price ? (
                      <Link
                        className="underline"
                        to={`/catalog/prices/${o.price}`}
                      >
                        {shortId(o.price, 14)}
                      </Link>
                    ) : (
                      <AdminMessage id="Unmapped" />
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    {canUpdate && !o.price && (
                      <MapButton onClick={() => setMapping(o.ref)} />
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
      {mapping && (
        <MapPriceDialog
          psp={psp}
          priceRef={mapping}
          onClose={() => setMapping(undefined)}
        />
      )}
    </Card>
  )
}

// ImportActions re-links (after mappings or a mapped plan) and applies. Apply
// lands only linked records and is refused while the import's job runs.
function ImportActions({ imp, psp }: { imp: PSPImport; psp?: PSP }) {
  const { text } = useAdminUI()
  const { importMutations, toast, toastApiError } = useAdminServices()
  const queryClient = useQueryClient()
  const canUpdate = useMerchantConfigUpdates()
  const relink = useMutation(importMutations.link(queryClient))
  const apply = useMutation(importMutations.apply(queryClient))
  const [confirming, setConfirming] = React.useState(false)
  if (!canUpdate) return null
  const ready = imp.status === "pulled" || imp.status === "applied"
  const again = imp.status === "applied"
  const word = psp?.key ?? imp.psp
  return (
    <div className="flex gap-2">
      <Button
        variant="outline"
        disabled={!ready || relink.isPending || importBusy(imp.status)}
        onClick={async () => {
          try {
            await relink.mutateAsync({ id: imp.id, params: {} })
            toast.success(text("Re-link requested"))
          } catch (err) {
            toastApiError(err, text("Re-link"))
          }
        }}
      >
        <AdminMessage id="Re-link" />
      </Button>
      <Button
        disabled={!ready || apply.isPending}
        onClick={() => setConfirming(true)}
      >
        {again ? text("Apply again") : text("Apply")}
      </Button>
      <TypedConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={again ? text("Apply again") : text("Apply import")}
        description={`${text("Lands the linked records as billing. ")}${recordCount(imp, "linked")}${text(" linked; ")}${recordCount(imp, "blocked")}${text(" blocked and ")}${recordCount(imp, "unmatched") + recordCount(imp, "conflict")}${text(" unmatched or conflicting stay staged.")}`}
        confirmationWord={word}
        actionLabel={again ? text("Apply again") : text("Apply")}
        onConfirm={async () => {
          try {
            await apply.mutateAsync(imp.id)
            toast.success(text("Apply queued"))
          } catch (err) {
            toastApiError(err, text("Apply import"))
          }
        }}
      />
    </div>
  )
}
