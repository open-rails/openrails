import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
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
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import type { Rail, SubscriptionStatus } from "@/lib/api/types"
import { ApiError, getTokens } from "@/lib/api/client"
import { DIALOG_WIDE } from "@/lib/dialog-width"
import { formatDate, formatNativeAmount } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"
import { toastApiError } from "@/lib/toast"
import {
  adminTierChangeBlockReason,
  tierChangeOptionLabel,
  tierChangeOptions,
} from "@/pages/subscriptions/tier-change-options"

interface ChangeTierDialogProps {
  subscriptionId: string
  customerId?: string
  productId: string
  priceId: string
  currency?: string
  scheduledPriceId?: string | null
  hasPendingReprice: boolean
  rail: Rail
  status: SubscriptionStatus
}

export function ChangeTierDialog(props: ChangeTierDialogProps) {
  return (
    <ChangeTierForm
      key={`${getTokens()?.merchant ?? ""}:${props.subscriptionId}`}
      {...props}
    />
  )
}

function ChangeTierForm({
  subscriptionId,
  customerId,
  productId,
  priceId,
  currency,
  scheduledPriceId,
  hasPendingReprice,
  rail,
  status,
}: ChangeTierDialogProps) {
  const [open, setOpen] = React.useState(false)
  const [selectedPriceId, setSelectedPriceId] = React.useState("")
  const [reviewedPriceId, setReviewedPriceId] = React.useState("")
  // A dismissed dialog or another preview cannot establish non-execution.
  // Retain each submitted request's key until its outcome is definitive.
  const attempts = React.useRef(new Map<string, string>())
  const view = React.useRef(0)
  React.useEffect(
    () => () => {
      view.current++
    },
    []
  )
  const queryClient = useQueryClient()
  const preview = useMutation(
    adminMutations.previewSubscriptionTierChange(subscriptionId)
  )
  const change = useMutation(
    adminMutations.changeSubscriptionTier(
      queryClient,
      subscriptionId,
      customerId
    )
  )
  const productsQuery = useQuery({
    ...adminQueries.allProducts({ errorAction: "Load tier-change plans" }),
    enabled: open,
  })
  const pricesQuery = useQuery({
    ...adminQueries.allPrices({ errorAction: "Load tier-change prices" }),
    enabled: open,
  })

  const products = productsQuery.data?.items ?? []
  const prices = pricesQuery.data?.items ?? []
  const currentProduct = products.find((product) => product.id === productId)
  const currentPrice = prices.find((price) => price.id === priceId)
  const options = tierChangeOptions({
    currentProduct,
    currentCurrency: currency ?? currentPrice?.currency,
    products,
    prices,
  })
  const selected = options.find((option) => option.price.id === selectedPriceId)
  const reviewed =
    reviewedPriceId === selectedPriceId ? preview.data : undefined
  const blockReason = adminTierChangeBlockReason({
    rail,
    status,
    scheduledPriceId,
    hasPendingReprice,
  })

  const handleOpenChange = (next: boolean) => {
    view.current++
    setOpen(next)
  }

  const handleSelect = (value: string | null) => {
    view.current++
    setSelectedPriceId(value ?? "")
    setReviewedPriceId("")
    preview.reset()
    change.reset()
  }

  const previewError =
    preview.error instanceof Error ? preview.error.message : ""
  const changeError = change.error instanceof Error ? change.error.message : ""
  const responseMessage = change.data?.message
  const responseURL = change.data?.url ?? change.data?.payment.redirect_url

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          <Button
            variant="outline"
            size="sm"
            disabled={Boolean(blockReason) && !change.variables}
            title={blockReason}
          >
            Change tier
          </Button>
        }
      />
      <DialogContent className={DIALOG_WIDE}>
        <DialogHeader>
          <DialogTitle>Change subscription tier</DialogTitle>
          <DialogDescription>
            Choose a plan in the same tier group, then review the charge and
            effective time.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-5">
          <div className="grid gap-1.5">
            <Label htmlFor="subscription-tier">New plan</Label>
            <Select value={selectedPriceId} onValueChange={handleSelect}>
              <SelectTrigger
                id="subscription-tier"
                className="w-full"
                disabled={productsQuery.isPending || pricesQuery.isPending}
              >
                <SelectValue
                  placeholder={
                    productsQuery.isPending || pricesQuery.isPending
                      ? "Loading plans…"
                      : "Choose a plan"
                  }
                />
              </SelectTrigger>
              <SelectContent>
                {options.map((option) => (
                  <SelectItem key={option.price.id} value={option.price.id}>
                    {tierChangeOptionLabel(option)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {!productsQuery.isPending &&
              !pricesQuery.isPending &&
              !productsQuery.isError &&
              !pricesQuery.isError &&
              options.length === 0 && (
                <p className="text-xs text-muted-foreground">
                  No eligible recurring plans share this tier group and
                  currency.
                </p>
              )}
            {(productsQuery.isError || pricesQuery.isError) && (
              <p className="text-xs text-destructive" role="alert">
                Could not load the available plans.
              </p>
            )}
          </div>

          {reviewed && selected && (
            <div className="grid gap-3" aria-live="polite">
              <div className="flex items-center justify-between gap-4">
                <span className="text-sm font-medium">Review</span>
                <Badge variant="secondary">{reviewed.action}</Badge>
              </div>
              <dl className="grid grid-cols-2 gap-x-8 gap-y-3 border-y py-4 text-sm">
                <div className="grid gap-1">
                  <dt className="text-xs text-muted-foreground">Due now</dt>
                  <dd className="font-medium tabular-nums">
                    {formatNativeAmount(
                      reviewed.amount_due_now,
                      reviewed.currency
                    )}
                  </dd>
                </div>
                <div className="grid gap-1">
                  <dt className="text-xs text-muted-foreground">Next charge</dt>
                  <dd className="font-medium tabular-nums">
                    {formatNativeAmount(
                      reviewed.next_charge_amount,
                      reviewed.currency
                    )}
                  </dd>
                </div>
                <div className="grid gap-1">
                  <dt className="text-xs text-muted-foreground">Effective</dt>
                  <dd className="font-medium">
                    {reviewed.effective === "now"
                      ? "Immediately"
                      : formatDate(reviewed.next_charge_date)}
                  </dd>
                </div>
                <div className="grid gap-1">
                  <dt className="text-xs text-muted-foreground">
                    Next billing date
                  </dt>
                  <dd className="font-medium tabular-nums">
                    {formatDate(reviewed.next_charge_date)}
                  </dd>
                </div>
              </dl>
              {reviewed.is_estimate && (
                <p className="text-xs text-muted-foreground">
                  The payment provider calculates the final prorated amount.
                </p>
              )}
            </div>
          )}

          {(previewError || changeError) && (
            <p className="text-sm text-destructive" role="alert">
              {changeError || previewError}
            </p>
          )}

          {change.data && change.data.status !== "succeeded" && (
            <div
              className="grid gap-2 rounded-lg bg-muted px-3 py-2.5 text-sm"
              aria-live="polite"
            >
              <p>{responseMessage ?? "This change needs another step."}</p>
              {responseURL && (
                <a
                  href={responseURL}
                  target="_blank"
                  rel="noreferrer"
                  className="w-fit font-medium text-primary underline underline-offset-4"
                >
                  Continue with payment provider
                </a>
              )}
            </div>
          )}
        </div>

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            onClick={() => handleOpenChange(false)}
          >
            Cancel
          </Button>
          {!reviewed ? (
            <Button
              type="button"
              disabled={!selectedPriceId || preview.isPending}
              onClick={async () => {
                const currentView = view.current
                try {
                  await preview.mutateAsync(selectedPriceId)
                  if (view.current !== currentView) return
                  setReviewedPriceId(selectedPriceId)
                } catch (error) {
                  if (view.current === currentView) {
                    toastApiError(error, "Preview tier change")
                  }
                }
              }}
            >
              {preview.isPending ? "Reviewing…" : "Review change"}
            </Button>
          ) : (
            <Button
              type="button"
              disabled={
                change.isPending ||
                (Boolean(change.data) && change.data?.status !== "processing")
              }
              onClick={async () => {
                const currentView = view.current
                const changeKey =
                  attempts.current.get(selectedPriceId) ?? crypto.randomUUID()
                attempts.current.set(selectedPriceId, changeKey)
                const completeAttempt = () => {
                  if (attempts.current.get(selectedPriceId) === changeKey) {
                    attempts.current.delete(selectedPriceId)
                  }
                }
                try {
                  const result = await change.mutateAsync({
                    priceId: selectedPriceId,
                    idempotencyKey: changeKey,
                  })
                  if (
                    result.status === "succeeded" ||
                    result.status === "blocked"
                  ) {
                    completeAttempt()
                  }
                  if (view.current !== currentView) return
                  if (result.status === "succeeded") {
                    toast.success(
                      result.action === "upgrade"
                        ? "Subscription upgraded"
                        : "Downgrade scheduled"
                    )
                    handleOpenChange(false)
                    setSelectedPriceId("")
                    setReviewedPriceId("")
                    preview.reset()
                    change.reset()
                  }
                  if (result.status === "processing") {
                    toast.info(
                      "The provider is still confirming this change. Check again to read the result."
                    )
                  }
                } catch (error) {
                  // 402 is a final provider refusal regardless of its code.
                  // Other refusals can reject a readback of an already accepted
                  // operation, so retire only explicitly terminal outcomes.
                  if (
                    error instanceof ApiError &&
                    (error.status === 402 ||
                      (error.status === 409 &&
                        (error.code === "tier_change_refused" ||
                          error.code === "tier_change_idempotency_conflict")))
                  ) {
                    completeAttempt()
                  }
                  if (view.current === currentView) {
                    toastApiError(error, "Change subscription tier")
                  }
                }
              }}
            >
              {change.isPending
                ? "Applying…"
                : change.data
                  ? change.data.status === "processing"
                    ? "Check result"
                    : change.data.status === "blocked"
                      ? "Change blocked"
                      : "Action required"
                  : `Confirm ${reviewed.action}`}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
