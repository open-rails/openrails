import { useQuery } from "@tanstack/react-query"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import type {
  CatalogPrice,
  CatalogProviderState,
  CheckoutRoutingSkip,
} from "@/lib/api/types"
import { formatDate } from "@/lib/format"
import { adminQueries } from "@/lib/queries"

// or#812 — the frontend contract for catalog psp_links.
//
// The catalog is DECLARATIVE: OpenRails definitions are pushed to a provider by
// the provider adapter, and the reconciliation job that reads back is
// alert-only. So this surface DISPLAYS a price's psp_links and their
// provider-side state; it never edits them. The write path is the catalog
// (manifest / create-price `providers`), not a per-link form.
//
// Two questions get answered side by side, and they are different questions:
//   - Link state (`price.providers`, the psp_links projection): does OpenRails
//     hold a link entry for this PSP, and does the remote object still match?
//   - Checkout readiness (or#288 routing dry run): would a real checkout
//     actually land here right now? A price can be perfectly linked and still
//     be unroutable because the PSP is unarmed or its credentials are missing.

// SKIP_LABELS is the or#288 skip vocabulary, verbatim keys, rendered for
// operators. Keep the keys in lockstep with
// internal/db/models/checkout_session.go.
const SKIP_LABELS: Record<CheckoutRoutingSkip, string> = {
  unknown_selector: "That name matches no provider you have set up.",
  ambiguous_selector:
    "More than one provider on this rail is set up. Name the provider rather than the rail.",
  not_armed: "This provider is not set up here, or it was archived.",
  credentials_missing:
    "The provider is set up, but the credentials it needs are missing.",
  link_missing: "This price has not been sent to the provider yet.",
  mode_unsupported:
    "This provider cannot take this kind of payment (one-off or recurring).",
  service_unavailable: "The connection to this provider is not running.",
  resolve_failed: "The provider could not be worked out at the time of asking.",
}

// The engine names each outcome for its own logs. These are the same outcomes
// as a sentence you can act on.
const SKIP_HEADLINES: Record<CheckoutRoutingSkip, string> = {
  unknown_selector: "unknown provider",
  ambiguous_selector: "more than one match",
  not_armed: "not set up",
  credentials_missing: "credentials missing",
  link_missing: "not sent to provider",
  mode_unsupported: "cannot take this payment",
  service_unavailable: "connection not running",
  resolve_failed: "could not be worked out",
}

function skipHeadline(skip?: string): string {
  if (!skip) return "ready"
  return SKIP_HEADLINES[skip as CheckoutRoutingSkip] ?? skip
}

function skipLabel(skip?: string) {
  if (!skip) return ""
  return SKIP_LABELS[skip as CheckoutRoutingSkip] ?? skip
}

const OK_BADGE = "bg-settled-surface text-settled"
const WARN_BADGE = "bg-held-surface text-held"
const BAD_BADGE = "bg-failed-surface text-failed"

function linkStatusClass(status: CatalogProviderState["status"]) {
  if (status === "linked") return OK_BADGE
  if (status === "error") return BAD_BADGE
  return WARN_BADGE
}

// syncStatusClass: "unknown" is deliberately NEUTRAL, not a warning — it means
// nobody has looked yet, which is the honest default until Verify runs.
function syncStatusClass(sync?: string) {
  switch (sync) {
    case "in_sync":
      return OK_BADGE
    case "drifted":
    case "missing":
      return BAD_BADGE
    case "never_synced":
    case "sync_disabled":
      return WARN_BADGE
    default:
      return ""
  }
}

// LINK_ID_ORDER puts the identifying fields first; anything else the adapter
// stored follows in sorted order. `rail` is pulled out into its own column.
const LINK_ID_ORDER = [
  "price_id",
  "product_id",
  "plan_id",
  "form_name",
  "flex_id",
  "recurring_billing_option_id",
  "lookup_key",
  "provider",
]

function linkEntries(ids?: Record<string, string>) {
  const rest = Object.entries(ids ?? {}).filter(
    ([k, v]) => k !== "rail" && v !== ""
  )
  rest.sort(([a], [b]) => {
    const ia = LINK_ID_ORDER.indexOf(a)
    const ib = LINK_ID_ORDER.indexOf(b)
    if (ia !== ib)
      return (
        (ia < 0 ? LINK_ID_ORDER.length : ia) -
        (ib < 0 ? LINK_ID_ORDER.length : ib)
      )
    return a.localeCompare(b)
  })
  return rest
}

export function PSPLinksCard({
  price,
  verifying,
  verified,
  onVerify,
}: {
  price: CatalogPrice
  verifying: boolean
  verified: boolean
  onVerify: () => void
}) {
  const links = Object.entries(price.providers ?? {}).sort(([a], [b]) =>
    a.localeCompare(b)
  )
  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-2">
        <div className="grid gap-1">
          <CardTitle className="text-sm">Payment providers</CardTitle>
          <CardDescription>
            Which providers this price has been sent to, and whether their copy
            still matches yours. Publishing the catalog is what sends it; this
            page only reports. Verify asks each provider what it currently
            holds, and changes nothing.
          </CardDescription>
        </div>
        <Button
          variant="outline"
          size="sm"
          disabled={verifying || !links.length}
          onClick={onVerify}
        >
          {verifying ? "Verifying…" : "Verify"}
        </Button>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {!links.length ? (
          <p className="text-sm text-muted-foreground">
            This price has not been sent to any provider, so nobody can be
            charged for it yet.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Provider</TableHead>
                  <TableHead>Rail</TableHead>
                  <TableHead>Sent</TableHead>
                  <TableHead>Still matches</TableHead>
                  <TableHead>Their reference</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {links.map(([psp, state]) => (
                  <TableRow key={psp}>
                    <TableCell className="font-mono text-xs">{psp}</TableCell>
                    <TableCell className="text-xs">
                      {state.ids?.rail ?? "—"}
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant="secondary"
                        className={linkStatusClass(state.status)}
                      >
                        {state.status}
                      </Badge>
                      {state.message && (
                        <p className="mt-1 max-w-xs text-xs text-muted-foreground">
                          {state.message}
                        </p>
                      )}
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant="secondary"
                        className={syncStatusClass(state.sync_status)}
                      >
                        {state.sync_status ?? "unknown"}
                      </Badge>
                      {state.last_synced_at && (
                        <p className="mt-1 text-xs text-muted-foreground">
                          {formatDate(state.last_synced_at)}
                        </p>
                      )}
                      {state.drift?.map((d) => (
                        <p
                          key={d.field}
                          className="mt-1 text-xs text-muted-foreground"
                        >
                          {d.field}: yours {d.openrails_value} · theirs{" "}
                          {d.remote_value}
                        </p>
                      ))}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {linkEntries(state.ids).length ? (
                        <div className="grid gap-0.5">
                          {linkEntries(state.ids).map(([k, v]) => (
                            <span key={k}>
                              {k}={v}
                            </span>
                          ))}
                        </div>
                      ) : (
                        "—"
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
        {!!price.pending_manual_actions?.length && (
          <div className="rounded-md border border-held/40 p-3 text-xs">
            {price.pending_manual_actions.map((a) => (
              <p key={`${a.provider}-${a.action}`}>
                <span className="font-mono">{a.provider}</span>:{" "}
                {a.hint || a.action}
              </p>
            ))}
          </div>
        )}
        {!verified && !!links.length && (
          <p className="text-xs text-muted-foreground">
            Nothing is claimed about the providers' copies until you press
            Verify.
          </p>
        )}
      </CardContent>
    </Card>
  )
}

// CheckoutReadinessCard answers the operator's real question — "can someone buy
// this right now, and if not, why?" — with the or#288 dry run, so the console
// and the routed checkout can never disagree.
export function CheckoutReadinessCard({ price }: { price: CatalogPrice }) {
  const {
    data: decision,
    isFetching: busy,
    refetch: reload,
  } = useQuery(adminQueries.checkoutRouting(price.id))

  const eligible = decision?.candidates.filter((c) => !c.skip) ?? []

  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-2">
        <div className="grid gap-1">
          <CardTitle className="text-sm">Can this be bought?</CardTitle>
          <CardDescription>
            Which provider would take the money if someone bought this right
            now, and why the others were passed over. This asks the same
            question a real checkout asks, without starting one.
          </CardDescription>
        </div>
        <Button
          variant="outline"
          size="sm"
          disabled={busy}
          onClick={() => void reload()}
        >
          {busy ? "Checking…" : "Re-check"}
        </Button>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {!decision ? (
          <p className="text-sm text-muted-foreground">
            {busy ? "Checking…" : "This check could not be run."}
          </p>
        ) : (
          <>
            <p className="text-sm">
              {decision.selected ? (
                <>
                  Money for this price would go to{" "}
                  <span className="font-medium">{decision.selected}</span>
                  {decision.rail && <> on {decision.rail}</>}.
                </>
              ) : (
                <span className="text-held">
                  No provider can take this payment, so nobody can buy this
                  price yet.
                </span>
              )}
            </p>
            {!decision.candidates.length ? (
              <p className="text-sm text-muted-foreground">
                There were no providers to consider.
              </p>
            ) : (
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Provider</TableHead>
                      <TableHead>Rail</TableHead>
                      <TableHead>Outcome</TableHead>
                      <TableHead>Why</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {decision.candidates.map((c) => (
                      <TableRow key={c.selector}>
                        <TableCell className="font-mono text-xs">
                          {c.selector}
                          {c.selector === decision.selected && (
                            <Badge
                              variant="secondary"
                              className={`ml-2 ${OK_BADGE}`}
                            >
                              selected
                            </Badge>
                          )}
                        </TableCell>
                        <TableCell className="text-xs">
                          {c.rail || "—"}
                        </TableCell>
                        <TableCell>
                          <Badge
                            variant="secondary"
                            className={c.skip ? WARN_BADGE : OK_BADGE}
                          >
                            {skipHeadline(c.skip)}
                          </Badge>
                        </TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          {skipLabel(c.skip)}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            )}
            {!!decision.candidates.length && eligible.length === 0 && (
              <p className="text-xs text-muted-foreground">
                "Not sent to provider" is fixed by publishing the catalog.
                Everything else is fixed under Settings, Payment providers.
              </p>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}
