import * as React from "react"
import { toast } from "sonner"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { confirmCopilotDraft, type PriceChangeDraft } from "@/lib/api/copilot"
import { createPrice, getMerchantSettings, previewRepriceAllPriorVersions, repriceAllPriorVersions } from "@/lib/api/endpoints"
import type { CatalogPrice } from "@/lib/api/types"
import { formatMicros, microsFromInput } from "@/lib/format"
import { toastApiError } from "@/lib/toast"
import { priceIntervalLabel } from "@/pages/catalog/price-format"
import {
  buildReviewText,
  defaultEffectiveDate,
  defaultMigrationMode,
  DEFAULT_NOTICE_WINDOW_DAYS,
  detectDirection,
  isEffectiveDateValid,
  minEffectiveDate,
  type MigrationMode,
} from "@/pages/catalog/price-wizard-logic"

const toDateInputValue = (d: Date) => d.toISOString().slice(0, 10)

// PriceChangeWizard is the #777 console price-change wizard — the ONLY
// amount-edit affordance on a price. Three steps: new amount -> migration
// plan (direction-aware defaults, live affected-count preview, notice-window
// gate on increase+migrate) -> review in words, then confirm applies the
// catalog price update followed (if migrating) by the reprice schedule.
//
// draft (#779): when the catalog copilot proposed this change, the dialog
// opens PRE-FILLED straight at Step 3 (review) — the affected count already
// rode in the draft, so no extra preview call is needed. Confirm behaves
// EXACTLY as a hand-typed change (same createPrice/repriceAllPriorVersions
// calls); the only addition is a best-effort audit-provenance log after
// success, never before, never blocking the real confirm.
export function PriceChangeWizard({
  price,
  productName,
  draft,
  onDone,
}: {
  price: CatalogPrice
  productName: string
  draft?: PriceChangeDraft
  onDone: () => void
}) {
  const [open, setOpen] = React.useState(() => !!draft)
  const [step, setStep] = React.useState<1 | 2 | 3>(() => (draft ? 3 : 1))
  const [amountInput, setAmountInput] = React.useState(() =>
    String((draft?.new_amount ?? price.unit_amount) / 1_000_000),
  )
  const [mode, setMode] = React.useState<MigrationMode>(() => draft?.migration_mode ?? "grandfather")
  const [effectiveAt, setEffectiveAt] = React.useState(() =>
    draft?.reprice ? toDateInputValue(new Date(draft.reprice.effective_at)) : "",
  )
  const [previewLoading, setPreviewLoading] = React.useState(false)
  const [affectedCount, setAffectedCount] = React.useState<number | null>(draft?.affected_count ?? null)
  const [busy, setBusy] = React.useState(false)
  // #781: the merchant-configurable notice window, read from the API (the
  // console used to hard-code 30 days here; the server is now the actual
  // boundary — GET /v1/merchant/settings — this is only the fail-fast UX
  // gate). DEFAULT_NOTICE_WINDOW_DAYS covers the brief window before the
  // fetch resolves.
  const [noticeWindowDays, setNoticeWindowDays] = React.useState(DEFAULT_NOTICE_WINDOW_DAYS)

  const [now] = React.useState(() => new Date())
  const newAmount = microsFromInput(amountInput) ?? price.unit_amount
  const direction = detectDirection(newAmount, price.unit_amount)

  const reset = () => {
    setStep(1)
    setAmountInput(String(price.unit_amount / 1_000_000))
    setMode("grandfather")
    setEffectiveAt("")
    setAffectedCount(null)
  }

  const handleOpenChange = (next: boolean) => {
    if (!next) {
      reset()
    } else {
      getMerchantSettings()
        .then((settings) => setNoticeWindowDays(settings.reprice_notice_window_days ?? DEFAULT_NOTICE_WINDOW_DAYS))
        .catch(() => {
          /* fail-soft: keep the default fallback — the server enforces the real window regardless. */
        })
    }
    setOpen(next)
  }

  const enterStep2 = () => {
    setMode(defaultMigrationMode(direction))
    setEffectiveAt(toDateInputValue(defaultEffectiveDate(direction, now, noticeWindowDays)))
    setStep(2)
    setPreviewLoading(true)
    setAffectedCount(null)
    previewRepriceAllPriorVersions(price.key)
      .then((res) => setAffectedCount(res.matched))
      .catch((err) => toastApiError(err, "Preview affected subscribers"))
      .finally(() => setPreviewLoading(false))
  }

  const minDate = minEffectiveDate(direction, now, noticeWindowDays)
  const dateValid =
    mode !== "migrate" || (!!effectiveAt && isEffectiveDateValid(direction, new Date(effectiveAt), now, noticeWindowDays))

  // Archived (prior-version) rows are history, not the editable current
  // price — editing rides the key's CURRENT row (GET .../prices/by-key/:key
  // always resolves it) so there is exactly one live edit target per key.
  if (price.archived) {
    return (
      <Button variant="outline" size="sm" disabled title="This is an archived prior version — change the current price for this key instead.">
        Change price
      </Button>
    )
  }

  const confirm = async () => {
    setBusy(true)
    try {
      const created = await createPrice({
        product_id: price.product_id,
        unit_amount: newAmount,
        currency: price.currency,
        access_duration_hours: price.access_duration_hours,
        auto_renew: price.auto_renew,
        trial_unit_amount: price.trial_unit_amount,
        trial_duration_hours: price.trial_duration_hours,
        key: price.key,
        providers: Object.keys(price.providers ?? {}),
      })
      if (mode === "migrate") {
        await repriceAllPriorVersions(price.key, new Date(effectiveAt).toISOString())
      }
      if (created.pending_manual_actions?.length) {
        toast.warning(
          `Price updated — manual step needed: ${created.pending_manual_actions.map((a) => `${a.provider}: ${a.hint}`).join("; ")}`,
        )
      } else {
        toast.success(mode === "migrate" ? "Price updated and migration scheduled" : "Price updated")
      }
      // Best-effort audit-provenance log: fires AFTER the real confirm above
      // already succeeded, never blocks or affects it.
      if (draft) {
        confirmCopilotDraft(draft.draft_id, "price_change", price.key).catch(() => {})
      }
      handleOpenChange(false)
      onDone()
    } catch (err) {
      toastApiError(err, "Change price")
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      {!draft && (
        <DialogTrigger asChild>
          <Button variant="outline" size="sm">
            Change price
          </Button>
        </DialogTrigger>
      )}
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            Change price — {productName}
            {draft && <Badge variant="secondary">drafted by copilot</Badge>}
          </DialogTitle>
          <DialogDescription>
            Step {step} of 3: {step === 1 ? "new amount" : step === 2 ? "migration plan" : "review"}
          </DialogDescription>
        </DialogHeader>

        {step === 1 && (
          <div className="grid gap-3">
            <div className="grid grid-cols-2 gap-3 text-sm">
              <div>
                <p className="text-xs text-muted-foreground uppercase">Current amount</p>
                <p className="font-medium">{formatMicros(price.unit_amount, price.currency)}</p>
              </div>
              <div>
                <p className="text-xs text-muted-foreground uppercase">Currency · interval</p>
                <p className="font-medium">
                  {price.currency.toUpperCase()} · {priceIntervalLabel(price)}
                </p>
              </div>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="wiz-amount">New amount (major units)</Label>
              <Input
                id="wiz-amount"
                type="number"
                step="any"
                min="0"
                value={amountInput}
                onChange={(e) => setAmountInput(e.target.value)}
                autoFocus
              />
            </div>
            {direction !== "unchanged" && (
              <p className="text-sm text-muted-foreground">
                This is a price{" "}
                <span className="font-medium text-foreground">{direction === "increase" ? "increase" : "decrease"}</span>.
                Currency and interval stay locked to the current price.
              </p>
            )}
          </div>
        )}

        {step === 2 && (
          <div className="grid gap-3">
            <p className="text-sm">
              {affectedCount === null && previewLoading
                ? "Checking affected subscribers…"
                : `${(affectedCount ?? 0).toLocaleString()} active subscription${affectedCount === 1 ? "" : "s"} on prior versions of ${price.key}.`}
            </p>
            <div className="grid gap-2">
              <label className="flex items-start gap-2 rounded-md border p-3 text-sm">
                <input
                  type="radio"
                  name="migration-mode"
                  className="mt-1"
                  checked={mode === "grandfather"}
                  onChange={() => setMode("grandfather")}
                />
                <span>
                  <span className="font-medium">Grandfather</span> — existing subscribers keep the current price.
                  {direction === "increase" && <Badge variant="secondary" className="ml-2">default</Badge>}
                </span>
              </label>
              <label className="flex items-start gap-2 rounded-md border p-3 text-sm">
                <input
                  type="radio"
                  name="migration-mode"
                  className="mt-1"
                  checked={mode === "migrate"}
                  onChange={() => setMode("migrate")}
                />
                <span>
                  <span className="font-medium">Migrate</span> — move everyone to the new price at their next renewal
                  on/after a date.
                  {direction === "decrease" && <Badge variant="secondary" className="ml-2">default</Badge>}
                </span>
              </label>
            </div>
            {mode === "migrate" && (
              <div className="grid gap-1.5">
                <Label htmlFor="wiz-effective">Effective date</Label>
                <Input
                  id="wiz-effective"
                  type="date"
                  value={effectiveAt}
                  min={minDate ? toDateInputValue(minDate) : undefined}
                  onChange={(e) => setEffectiveAt(e.target.value)}
                />
                {direction === "increase" ? (
                  <p className="text-xs text-muted-foreground">
                    Must be at least {noticeWindowDays} days out (card-network advance-notice window for amount
                    increases — configurable in Settings; enforced by the API).
                  </p>
                ) : (
                  <p className="text-xs text-muted-foreground">
                    Decreases have no notice requirement — today is fine.
                  </p>
                )}
                {!dateValid && <p className="text-xs text-destructive">Pick a later date.</p>}
                <p className="text-xs text-muted-foreground">
                  A reprice_scheduled notice is queued for every affected subscriber as soon as you confirm.
                </p>
              </div>
            )}
          </div>
        )}

        {step === 3 && (
          <div className="grid gap-3">
            <p className="rounded-md border bg-muted/40 p-3 text-sm">
              {buildReviewText({
                newAmount,
                currentAmount: price.unit_amount,
                currency: price.currency,
                affectedCount: affectedCount ?? 0,
                plan: { mode, effectiveAt: mode === "migrate" ? new Date(effectiveAt).toISOString() : "" },
                now,
              })}
            </p>
          </div>
        )}

        <DialogFooter>
          {step > 1 && (
            <Button variant="outline" disabled={busy} onClick={() => setStep((s) => (s - 1) as 1 | 2)}>
              Back
            </Button>
          )}
          {step === 1 && (
            <Button disabled={!amountInput || direction === "unchanged" || newAmount <= 0} onClick={enterStep2}>
              Next
            </Button>
          )}
          {step === 2 && (
            <Button disabled={!dateValid || previewLoading} onClick={() => setStep(3)}>
              Next
            </Button>
          )}
          {step === 3 && (
            <Button disabled={busy} onClick={confirm}>
              {busy ? "Working…" : "Confirm"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
