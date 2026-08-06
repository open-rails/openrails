import * as React from "react"
import { toast } from "sonner"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { FormFieldErrors } from "@/components/form-field-errors"
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
import type { PriceChangeDraft } from "@/lib/api/copilot"
import type { CatalogPrice } from "@/lib/api/types"
import { formatMicros, microsFromInput } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { toastApiError } from "@/lib/toast"
import { adminQueries } from "@/lib/queries"
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

const priceChangeFormValues = (
  price: CatalogPrice,
  draft?: PriceChangeDraft
) => ({
  amountInput: String((draft?.new_amount ?? price.unit_amount) / 1_000_000),
  mode: (draft?.migration_mode ?? "grandfather") as MigrationMode,
  effectiveAt: draft?.reprice
    ? toDateInputValue(new Date(draft.reprice.effective_at))
    : "",
})

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
  onDone?: () => void
}) {
  const queryClient = useQueryClient()
  const [open, setOpen] = React.useState(() => !!draft)
  const [step, setStep] = React.useState<1 | 2 | 3>(() => (draft ? 3 : 1))
  const previewPriceChange = useMutation(adminMutations.previewPriceChange())
  const changePrice = useMutation(adminMutations.changePrice(queryClient))
  const affectedCount =
    previewPriceChange.data?.matched ?? draft?.affected_count ?? null
  const { data: merchantSettings } = useQuery({
    ...adminQueries.merchantSettings(),
    enabled: open,
  })
  const noticeWindowDays =
    merchantSettings?.reprice_notice_window_days ?? DEFAULT_NOTICE_WINDOW_DAYS

  const [now] = React.useState(() => new Date())
  const form = useForm({
    defaultValues: priceChangeFormValues(price, draft),
    onSubmit: async ({ value }) => {
      const newAmount = microsFromInput(value.amountInput)
      if (!newAmount || newAmount <= 0) return

      try {
        const created = await changePrice.mutateAsync({
          price: {
            product_id: price.product_id,
            unit_amount: newAmount,
            currency: price.currency,
            access_duration_hours: price.access_duration_hours,
            auto_renew: price.auto_renew,
            trial_unit_amount: price.trial_unit_amount,
            trial_duration_hours: price.trial_duration_hours,
            key: price.key,
            providers: Object.keys(price.providers ?? {}),
          },
          migration:
            value.mode === "migrate"
              ? {
                  priceKey: price.key,
                  effectiveAt: new Date(value.effectiveAt).toISOString(),
                }
              : undefined,
          copilotDraftId: draft?.draft_id,
        })
        if (created.pending_manual_actions?.length) {
          toast.warning(
            `Price updated — manual step needed: ${created.pending_manual_actions.map((action) => `${action.provider}: ${action.hint}`).join("; ")}`
          )
        } else {
          toast.success(
            value.mode === "migrate"
              ? "Price updated and migration scheduled"
              : "Price updated"
          )
        }
        handleOpenChange(false)
        onDone?.()
      } catch (err) {
        toastApiError(err, "Change price")
      }
    },
  })

  const reset = () => {
    setStep(1)
    form.reset(priceChangeFormValues(price))
    previewPriceChange.reset()
  }

  const handleOpenChange = (next: boolean) => {
    if (!next) {
      reset()
    }
    setOpen(next)
  }

  const enterStep2 = () => {
    const amount = microsFromInput(form.state.values.amountInput)
    const direction = detectDirection(
      amount ?? price.unit_amount,
      price.unit_amount
    )
    form.setFieldValue("mode", defaultMigrationMode(direction))
    form.setFieldValue(
      "effectiveAt",
      toDateInputValue(defaultEffectiveDate(direction, now, noticeWindowDays))
    )
    setStep(2)
    previewPriceChange.reset()
    previewPriceChange.mutate(price.key, {
      onError: (err) => toastApiError(err, "Preview affected subscribers"),
    })
  }

  // Archived (prior-version) rows are history, not the editable current
  // price — editing rides the key's CURRENT row (GET .../prices/by-key/:key
  // always resolves it) so there is exactly one live edit target per key.
  if (price.archived) {
    return (
      <Button
        variant="outline"
        size="sm"
        disabled
        title="This is an archived prior version — change the current price for this key instead."
      >
        Change price
      </Button>
    )
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      {!draft && (
        <DialogTrigger
          render={
            <Button variant="outline" size="sm">
              Change price
            </Button>
          }
        />
      )}
      <DialogContent className="max-w-lg">
        <form
          className="grid gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            if (step !== 3) return
            void form.handleSubmit()
          }}
        >
          <form.Subscribe
            selector={(state) => [state.values, state.isSubmitting] as const}
          >
            {([values, isSubmitting]) => {
              const newAmount =
                microsFromInput(values.amountInput) ?? price.unit_amount
              const direction = detectDirection(newAmount, price.unit_amount)
              const minDate = minEffectiveDate(direction, now, noticeWindowDays)
              const dateValid =
                values.mode !== "migrate" ||
                (!!values.effectiveAt &&
                  isEffectiveDateValid(
                    direction,
                    new Date(values.effectiveAt),
                    now,
                    noticeWindowDays
                  ))

              return (
                <>
                  <DialogHeader>
                    <DialogTitle className="flex items-center gap-2">
                      Change price — {productName}
                      {draft && (
                        <Badge variant="secondary">drafted by copilot</Badge>
                      )}
                    </DialogTitle>
                    <DialogDescription>
                      Step {step} of 3:{" "}
                      {step === 1
                        ? "new amount"
                        : step === 2
                          ? "migration plan"
                          : "review"}
                    </DialogDescription>
                  </DialogHeader>

                  {step === 1 && (
                    <div className="grid gap-3">
                      <div className="grid grid-cols-2 gap-3 text-sm">
                        <div>
                          <p className="text-xs text-muted-foreground uppercase">
                            Current amount
                          </p>
                          <p className="font-medium">
                            {formatMicros(price.unit_amount, price.currency)}
                          </p>
                        </div>
                        <div>
                          <p className="text-xs text-muted-foreground uppercase">
                            Currency · interval
                          </p>
                          <p className="font-medium">
                            {price.currency.toUpperCase()} ·{" "}
                            {priceIntervalLabel(price)}
                          </p>
                        </div>
                      </div>
                      <form.Field
                        name="amountInput"
                        validators={{
                          onBlur: ({ value }) => {
                            const amount = microsFromInput(value)
                            if (!amount || amount <= 0) {
                              return "Enter an amount greater than zero."
                            }
                            return undefined
                          },
                        }}
                      >
                        {(field) => (
                          <div className="grid gap-1.5">
                            <Label htmlFor="wiz-amount">
                              New amount (major units)
                            </Label>
                            <Input
                              id="wiz-amount"
                              type="number"
                              step="any"
                              min="0"
                              value={field.state.value}
                              onBlur={field.handleBlur}
                              onChange={(event) =>
                                field.handleChange(event.target.value)
                              }
                              autoFocus
                            />
                            <FormFieldErrors errors={field.state.meta.errors} />
                          </div>
                        )}
                      </form.Field>
                      {direction !== "unchanged" && (
                        <p className="text-sm text-muted-foreground">
                          This is a price{" "}
                          <span className="font-medium text-foreground">
                            {direction === "increase" ? "increase" : "decrease"}
                          </span>
                          . Currency and interval stay locked to the current
                          price.
                        </p>
                      )}
                    </div>
                  )}

                  {step === 2 && (
                    <div className="grid gap-3">
                      <p className="text-sm">
                        {affectedCount === null && previewPriceChange.isPending
                          ? "Checking affected subscribers…"
                          : `${(affectedCount ?? 0).toLocaleString()} active subscription${affectedCount === 1 ? "" : "s"} on prior versions of ${price.key}.`}
                      </p>
                      <form.Field name="mode">
                        {(field) => (
                          <div className="grid gap-2">
                            <label className="flex items-start gap-2 rounded-md border p-3 text-sm">
                              <input
                                type="radio"
                                name={field.name}
                                className="mt-1"
                                checked={field.state.value === "grandfather"}
                                onChange={() =>
                                  field.handleChange("grandfather")
                                }
                              />
                              <span>
                                <span className="font-medium">Grandfather</span>{" "}
                                — existing subscribers keep the current price.
                                {direction === "increase" && (
                                  <Badge variant="secondary" className="ml-2">
                                    default
                                  </Badge>
                                )}
                              </span>
                            </label>
                            <label className="flex items-start gap-2 rounded-md border p-3 text-sm">
                              <input
                                type="radio"
                                name={field.name}
                                className="mt-1"
                                checked={field.state.value === "migrate"}
                                onChange={() => field.handleChange("migrate")}
                              />
                              <span>
                                <span className="font-medium">Migrate</span> —
                                move everyone to the new price at their next
                                renewal on/after a date.
                                {direction === "decrease" && (
                                  <Badge variant="secondary" className="ml-2">
                                    default
                                  </Badge>
                                )}
                              </span>
                            </label>
                          </div>
                        )}
                      </form.Field>
                      {values.mode === "migrate" && (
                        <form.Field name="effectiveAt">
                          {(field) => (
                            <div className="grid gap-1.5">
                              <Label htmlFor="wiz-effective">
                                Effective date
                              </Label>
                              <Input
                                id="wiz-effective"
                                type="date"
                                value={field.state.value}
                                min={
                                  minDate
                                    ? toDateInputValue(minDate)
                                    : undefined
                                }
                                onBlur={field.handleBlur}
                                onChange={(event) =>
                                  field.handleChange(event.target.value)
                                }
                              />
                              {direction === "increase" ? (
                                <p className="text-xs text-muted-foreground">
                                  Must be at least {noticeWindowDays} days out
                                  (card-network advance-notice window for amount
                                  increases — configurable in Settings; enforced
                                  by the API).
                                </p>
                              ) : (
                                <p className="text-xs text-muted-foreground">
                                  Decreases have no notice requirement — today
                                  is fine.
                                </p>
                              )}
                              {!dateValid && (
                                <p
                                  className="text-xs text-destructive"
                                  role="alert"
                                >
                                  Pick a later date.
                                </p>
                              )}
                              <p className="text-xs text-muted-foreground">
                                A reprice_scheduled notice is queued for every
                                affected subscriber as soon as you confirm.
                              </p>
                            </div>
                          )}
                        </form.Field>
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
                          plan: {
                            mode: values.mode,
                            effectiveAt:
                              values.mode === "migrate"
                                ? new Date(values.effectiveAt).toISOString()
                                : "",
                          },
                          now,
                        })}
                      </p>
                    </div>
                  )}

                  <DialogFooter>
                    {step > 1 && (
                      <Button
                        type="button"
                        variant="outline"
                        disabled={isSubmitting}
                        onClick={() => setStep((value) => (value - 1) as 1 | 2)}
                      >
                        Back
                      </Button>
                    )}
                    {step === 1 && (
                      <Button
                        type="button"
                        disabled={
                          !values.amountInput ||
                          direction === "unchanged" ||
                          newAmount <= 0
                        }
                        onClick={enterStep2}
                      >
                        Next
                      </Button>
                    )}
                    {step === 2 && (
                      <Button
                        type="button"
                        disabled={!dateValid || previewPriceChange.isPending}
                        onClick={() => setStep(3)}
                      >
                        Next
                      </Button>
                    )}
                    {step === 3 && (
                      <Button type="submit" disabled={isSubmitting}>
                        {isSubmitting ? "Working…" : "Confirm"}
                      </Button>
                    )}
                  </DialogFooter>
                </>
              )
            }}
          </form.Subscribe>
        </form>
      </DialogContent>
    </Dialog>
  )
}
