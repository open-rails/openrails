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
  unknown_selector: "Not a declared PSP key or rail kind.",
  ambiguous_selector:
    "Bare rail kind matched more than one armed PSP — name the PSP key.",
  not_armed: "No active PSP row: never configured, or archived.",
  credentials_missing:
    "PSP is armed but the credentials this rail needs are absent.",
  link_missing: "This price carries no usable psp_link for the PSP.",
  mode_unsupported:
    "The rail cannot serve this checkout mode (one-off vs subscription).",
  service_unavailable: "The runtime service backing this rail is not wired.",
  resolve_failed: "The selector could not be resolved at decision time.",
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
          <CardTitle className="text-sm">PSP links</CardTitle>
          <CardDescription>
            The price's <code className="font-mono">psp_links</code>, keyed by
            PSP. Links are declared by the catalog and pushed by the provider
            adapter — they are not edited here. "Verify" performs a live
            read-only retrieve against each attached provider.
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
            No PSP links. This price exists in OpenRails only — no checkout can
            be routed to a provider for it.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>PSP</TableHead>
                  <TableHead>Rail</TableHead>
                  <TableHead>Link</TableHead>
                  <TableHead>Provider sync</TableHead>
                  <TableHead>Link ids</TableHead>
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
                          {d.field}: ours {d.openrails_value} · theirs{" "}
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
            Provider sync reads "unknown" until Verify runs — OpenRails does not
            claim a remote object matches without having looked.
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
          <CardTitle className="text-sm">Checkout readiness</CardTitle>
          <CardDescription>
            Where a checkout for this price would land right now, and why every
            other candidate was passed over. This runs the production routing
            decision — no session is created.
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
            {busy ? "Checking…" : "Routing could not be evaluated."}
          </p>
        ) : (
          <>
            <p className="text-sm">
              {decision.selected ? (
                <>
                  Selected{" "}
                  <span className="font-mono">{decision.selected}</span>
                  {decision.rail && <> on {decision.rail}</>}
                  {decision.mode && <> · {decision.mode}</>} · policy{" "}
                  {decision.policy}
                  {decision.rule !== undefined && <> (rule {decision.rule})</>}
                </>
              ) : (
                <span className="text-held">
                  No PSP is eligible — this price cannot be checked out.
                </span>
              )}
            </p>
            {!decision.candidates.length ? (
              <p className="text-sm text-muted-foreground">
                No PSP candidates were evaluated.
              </p>
            ) : (
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>PSP</TableHead>
                      <TableHead>Rail</TableHead>
                      <TableHead>Verdict</TableHead>
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
                            {c.skip ?? "eligible"}
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
                A <span className="font-mono">link_missing</span> verdict is a
                catalog problem (push the price to the provider); the other
                classes are PSP configuration — fix them under Settings →
                Payment providers.
              </p>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}
