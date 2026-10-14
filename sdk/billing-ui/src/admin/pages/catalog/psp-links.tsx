import { AdminMessage } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import { useQuery } from "@tanstack/react-query"

import { Badge } from "#orck/admin/components/ui/badge"
import { Button } from "#orck/admin/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
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
import type { PSPRoutingSkip } from "#orck/admin/lib/api/types"
import type { Price } from "#orck/client/generated/wire"

// A price is ours (OpenRails bills it, by card through the merchant's card
// vault and charge PSPs, and on Solana) or a provider's (the provider owns
// and runs it; OpenRails reads its terms). This surface reports which, and
// never edits a provider. It answers two questions:
//   - Ownership (`price.provider`): who runs the billing, and does a provider
//     price's provider still state the same terms?
//   - Purchase readiness (the routing preview): would a real purchase land
//     here now?

// SKIP_LABELS renders the routing skip vocabulary for operators. Keep the keys
// in lockstep with internal/db/models/order_attempt.go.
const SKIP_LABELS: Record<PSPRoutingSkip, string> = {
  unknown_selector: "That name matches no provider you have set up.",
  ambiguous_selector:
    "More than one provider on this rail is set up. Name the provider rather than the rail.",
  not_armed: "This provider is not set up here, or it was archived.",
  credentials_missing:
    "The provider is set up, but the credentials it needs are missing.",
  link_missing:
    "This provider has nothing to sell this price on: a CCBill FlexForm, or a published Solana plan.",
  not_sold_via: "The price is not sold this way: its sell_via leaves this method out.",
  not_a_charge_psp:
    "New card charges go to the merchant's charge PSPs; this provider is not one.",
  provider_price:
    "A provider price sells only through its own provider, and only CCBill still sells new ones.",
  mode_unsupported:
    "This provider cannot take this kind of payment (one-off or recurring).",
  posture_disarmed:
    "The provider's credentials failed their test-mode check, so it is switched off.",
  service_unavailable: "The connection to this provider is not running.",
  resolve_failed: "The provider could not be worked out at the time of asking.",
}

// The engine names each outcome for its own logs. These are the same outcomes
// as a sentence you can act on.
const SKIP_HEADLINES: Record<PSPRoutingSkip, string> = {
  unknown_selector: "unknown provider",
  ambiguous_selector: "more than one match",
  not_armed: "not set up",
  credentials_missing: "credentials missing",
  link_missing: "nothing to sell on",
  not_sold_via: "not sold this way",
  not_a_charge_psp: "not a charge PSP",
  provider_price: "provider's price",
  mode_unsupported: "cannot take this payment",
  posture_disarmed: "credentials failed check",
  service_unavailable: "connection not running",
  resolve_failed: "could not be worked out",
}

function skipHeadline(skip?: string | null): string {
  if (!skip) return "ready"
  return SKIP_HEADLINES[skip as PSPRoutingSkip] ?? skip
}

function skipLabel(skip?: string | null) {
  if (!skip) return ""
  return SKIP_LABELS[skip as PSPRoutingSkip] ?? skip
}

const OK_BADGE = "bg-settled-surface text-settled"
const WARN_BADGE = "bg-held-surface text-held"
const BAD_BADGE = "bg-failed-surface text-failed"

// syncStatusClass leaves "unknown" neutral, not a warning: nobody has checked
// yet.
function syncStatusClass(sync?: string) {
  switch (sync) {
    case "in_sync":
      return OK_BADGE
    case "drifted":
    case "missing":
      return BAD_BADGE
    case "sync_disabled":
      return WARN_BADGE
    default:
      return ""
  }
}

const ID_ORDER = [
  "plan_id",
  "price_id",
  "product_id",
  "flex_id",
  "recurring_billing_option_id",
]

function providerIDs(ids?: Record<string, string> | null) {
  return Object.entries(ids ?? {})
    .filter(([, v]) => v !== "")
    .sort(([a], [b]) => {
      const ia = ID_ORDER.indexOf(a)
      const ib = ID_ORDER.indexOf(b)
      return (
        (ia < 0 ? ID_ORDER.length : ia) - (ib < 0 ? ID_ORDER.length : ib) ||
        a.localeCompare(b)
      )
    })
}

// PriceOwnerCard shows who runs the price's billing: OpenRails, through the
// methods it sells by, or the provider that owns it, whose terms Verify reads
// again.
export function PriceOwnerCard({
  price,
  verifying,
  verified,
  onVerify,
}: {
  price: Price
  verifying: boolean
  verified: boolean
  onVerify: () => void
}) {
  const provider = price.provider
  if (!provider)
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-sm">
            <AdminMessage id={"Sold by OpenRails"} />
          </CardTitle>
          <CardDescription>
            <AdminMessage
              id={
                "OpenRails bills this price: by card through your card vault and charge PSPs, and on Solana. Nothing is written to a provider's catalog."
              }
            />
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-1 text-sm">
          <p>
            <AdminMessage id={"Sold via: "} />
            {price.sell_via?.length ? (
              price.sell_via.join(", ")
            ) : (
              <AdminMessage id={"every method you have"} />
            )}
          </p>
          {price.solana_plan && (
            <p className="font-mono text-xs">
              <AdminMessage id={"Solana plan "} />
              {price.solana_plan}
            </p>
          )}
        </CardContent>
      </Card>
    )
  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-2">
        <div className="grid gap-1">
          <CardTitle className="text-sm">
            <AdminMessage id={"Owned by its provider"} />
          </CardTitle>
          <CardDescription>
            <AdminMessage
              id={
                "The provider owns and runs this price; OpenRails read its terms and never writes it. Verify asks the provider what it holds now, and changes nothing."
              }
            />
          </CardDescription>
        </div>
        <Button variant="outline" size="sm" disabled={verifying} onClick={onVerify}>
          {verifying ? "Verifying…" : "Verify"}
        </Button>
      </CardHeader>
      <CardContent className="overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead className="text-muted-foreground">
                <AdminMessage id={"Provider"} />
              </TableHead>
              <TableHead className="text-muted-foreground">
                <AdminMessage id={"Rail"} />
              </TableHead>
              <TableHead className="text-muted-foreground">
                <AdminMessage id={"Still matches"} />
              </TableHead>
              <TableHead className="text-muted-foreground">
                <AdminMessage id={"Their reference"} />
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            <TableRow>
              <TableCell className="font-mono text-xs">{provider.psp}</TableCell>
              <TableCell className="text-xs">{provider.rail}</TableCell>
              <TableCell>
                <Badge
                  variant="secondary"
                  className={syncStatusClass(provider.sync_status)}
                >
                  {verified ? provider.sync_status : "unknown"}
                </Badge>
                {provider.drift?.map((d) => (
                  <p key={d.field} className="mt-1 text-xs text-muted-foreground">
                    {d.field}
                    <AdminMessage id={": yours "} />
                    {d.openrails_value}
                    <AdminMessage id={" · theirs"} /> {d.remote_value}
                  </p>
                ))}
              </TableCell>
              <TableCell className="font-mono text-xs">
                <div className="grid gap-0.5">
                  {providerIDs(provider.ids).map(([k, v]) => (
                    <span key={k}>
                      {k}={v}
                    </span>
                  ))}
                </div>
              </TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  )
}

// PSPReadinessCard answers "can someone buy this right now, and if not,
// why?" with the routing preview, so the console and purchase never disagree.
export function PSPReadinessCard({ price }: { price: Price }) {
  const { adminQueries } = useAdminServices()

  const {
    data: decision,
    isFetching: busy,
    refetch: reload,
  } = useQuery(adminQueries.pspRouting(price.id))

  const eligible = decision?.candidates.filter((c) => !c.skip) ?? []

  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-2">
        <div className="grid gap-1">
          <CardTitle className="text-sm">
            <AdminMessage id={"Can this be bought?"} />
          </CardTitle>
          <CardDescription>
            <AdminMessage
              id={
                "Which provider would take the money if someone bought this right now, and why the others were passed over. This asks the same question a real purchase asks, without starting one."
              }
            />
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
              {decision.psp ? (
                <>
                  <AdminMessage id={"Money for this price would go to"} />{" "}
                  <span className="font-medium">{decision.psp}</span>
                  {decision.rail && (
                    <>
                      <AdminMessage id={" on "} />
                      {decision.rail}
                    </>
                  )}
                  .
                </>
              ) : (
                <span className="text-held">
                  <AdminMessage
                    id={
                      "No provider can take this payment, so nobody can buy this price yet."
                    }
                  />
                </span>
              )}
            </p>
            {!decision.candidates.length ? (
              <p className="text-sm text-muted-foreground">
                <AdminMessage id={"There were no providers to consider."} />
              </p>
            ) : (
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent">
                      <TableHead className="text-muted-foreground">
                        <AdminMessage id={"Provider"} />
                      </TableHead>
                      <TableHead className="text-muted-foreground">
                        <AdminMessage id={"Rail"} />
                      </TableHead>
                      <TableHead className="text-muted-foreground">
                        <AdminMessage id={"Outcome"} />
                      </TableHead>
                      <TableHead className="text-muted-foreground">
                        <AdminMessage id={"Why"} />
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {decision.candidates.map((c) => (
                      <TableRow key={c.psp}>
                        <TableCell className="font-mono text-xs">
                          {c.psp}
                          {c.psp === decision.psp && (
                            <Badge
                              variant="secondary"
                              className={`ml-2 ${OK_BADGE}`}
                            >
                              <AdminMessage id={"selected"} />
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
                <AdminMessage
                  id={
                    '"Not sent to provider" is fixed by publishing the catalog. Everything else is fixed under Settings, Payment providers.'
                  }
                />
              </p>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}
