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
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import type {
  ChangeSubscriptionParams,
  Rail,
  ScheduledChange,
  SubscriptionStatus,
} from "@/lib/api/types"
import { ApiError, selectedMerchant } from "@/lib/api/client"
import { DIALOG_WIDE } from "@/lib/dialog-width"
import { formatDate, formatNativeAmount } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"
import { toastApiError } from "@/lib/toast"
import {
  adminSubscriptionChangeBlockReason,
  initialSeats,
  subscriptionChangeOptionLabel,
  subscriptionChangeOptions,
} from "@/pages/subscriptions/subscription-change-options"

interface ChangeSubscriptionDialogProps {
  subscriptionId: string
  customerId?: string
  productId: string
  priceId: string
  quantity: number | null
  currency?: string
  collectionPolicy?: string
  scheduledChange?: ScheduledChange | null
  rail: Rail
  status: SubscriptionStatus
}

export function ChangeSubscriptionDialog(props: ChangeSubscriptionDialogProps) {
  return (
    <ChangeSubscriptionForm
      key={`${selectedMerchant() ?? ""}:${props.subscriptionId}`}
      {...props}
    />
  )
}

function ChangeSubscriptionForm({
  subscriptionId,
  customerId,
  productId,
  priceId,
  quantity,
  currency,
  collectionPolicy,
  scheduledChange,
  rail,
  status,
}: ChangeSubscriptionDialogProps) {
  const [open, setOpen] = React.useState(false)
  const [selectedPriceId, setSelectedPriceId] = React.useState("")
  const [seats, setSeats] = React.useState("")
  const [reason, setReason] = React.useState("")
  const [reviewedKey, setReviewedKey] = React.useState("")
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
    adminMutations.previewSubscriptionChange(subscriptionId)
  )
  const change = useMutation(
    adminMutations.changeSubscription(
      queryClient,
      subscriptionId,
      customerId
    )
  )
  const productsQuery = useQuery({
    ...adminQueries.allProducts({ errorAction: "Load subscription plans" }),
    enabled: open,
  })
  const pricesQuery = useQuery({
    ...adminQueries.allPrices({ errorAction: "Load subscription prices" }),
    enabled: open,
  })

  const products = productsQuery.data?.data ?? []
  const prices = pricesQuery.data?.data ?? []
  const currentProduct = products.find((product) => product.id === productId)
  const currentPrice = prices.find((price) => price.id === priceId)
  const options = subscriptionChangeOptions({
    currentProduct,
    currentPrice,
    currentCurrency: currency ?? currentPrice?.currency,
    products,
    prices,
    scheduledChange,
  })
  const selected = options.find((option) => option.price.id === selectedPriceId)
  const bounds = selected?.price.quantity
  const seatCount = Number(seats)
  const seatsValid =
    !bounds ||
    (Number.isInteger(seatCount) &&
      seatCount >= bounds.min &&
      seatCount <= bounds.max)
  // The current price with its seats changes back from a scheduled change.
  const request: ChangeSubscriptionParams | undefined =
    selected && seatsValid
      ? {
          price_id: selected.price.id,
          ...(bounds ? { quantity: seatCount } : {}),
        }
      : undefined
  const unchanged =
    selected?.direction === "current" &&
    !scheduledChange &&
    (!bounds || seatCount === quantity)
  const requestKey = request ? JSON.stringify(request) : ""
  const reviewed =
    requestKey && reviewedKey === requestKey ? preview.data : undefined
  const blockReason = adminSubscriptionChangeBlockReason({
    rail,
    status,
    collectionPolicy,
    scheduledChange,
  })

  const handleOpenChange = (next: boolean) => {
    view.current++
    setOpen(next)
  }

  const handleSelect = (value: string | null) => {
    view.current++
    const price = options.find((option) => option.price.id === value)?.price
    setSelectedPriceId(value ?? "")
    setSeats(String(price ? (initialSeats(price, quantity) ?? "") : ""))
    setReviewedKey("")
    preview.reset()
    change.reset()
  }

  const handleSeats = (value: string) => {
    view.current++
    setSeats(value)
    setReviewedKey("")
    preview.reset()
    change.reset()
  }

  const previewError =
    preview.error instanceof Error ? preview.error.message : ""
  const changeError = change.error instanceof Error ? change.error.message : ""
  const responseMessage = change.data?.message
  const responseURL = change.data?.next_action?.url ?? undefined

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
            Change subscription
          </Button>
        }
      />
      <DialogContent className={DIALOG_WIDE}>
        <DialogHeader>
          <DialogTitle>Change subscription</DialogTitle>
          <DialogDescription>
            Change at the customer&apos;s request, as their own change would:
            an upgrade or more seats is charged to their card now; a downgrade
            or fewer seats waits for the next renewal. The customer is told.
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
                    {subscriptionChangeOptionLabel(option)}
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

          {bounds && (
            <div className="grid gap-1.5">
              <Label htmlFor="subscription-seats">Seats</Label>
              <Input
                id="subscription-seats"
                type="number"
                inputMode="numeric"
                min={bounds.min}
                max={bounds.max}
                value={seats}
                onChange={(event) => handleSeats(event.target.value)}
              />
              <p className="text-xs text-muted-foreground">
                {bounds.min} to {bounds.max} seats.
              </p>
            </div>
          )}

          <div className="grid gap-1.5">
            <Label htmlFor="subscription-change-reason">Reason</Label>
            <Input
              id="subscription-change-reason"
              value={reason}
              maxLength={500}
              placeholder="What the customer asked for"
              onChange={(event) => setReason(event.target.value)}
            />
            <p className="text-xs text-muted-foreground">
              Kept with the change and any charge.
            </p>
          </div>

          {reviewed && selected && (
            <div className="grid gap-3" aria-live="polite">
              <div className="flex items-center justify-between gap-4">
                <span className="text-sm font-medium">Review</span>
                <Badge variant="secondary">
                  {reviewed.effective === "now" ? "Immediate" : "At renewal"}
                </Badge>
              </div>
              <dl className="grid grid-cols-2 gap-x-8 gap-y-3 border-y py-4 text-sm">
                {reviewed.quantity !== null && (
                  <div className="grid gap-1">
                    <dt className="text-xs text-muted-foreground">Seats</dt>
                    <dd className="font-medium tabular-nums">
                      {reviewed.quantity}
                    </dd>
                  </div>
                )}
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
              disabled={!request || unchanged || preview.isPending}
              onClick={async () => {
                if (!request) return
                const currentView = view.current
                try {
                  await preview.mutateAsync(request)
                  if (view.current !== currentView) return
                  setReviewedKey(requestKey)
                } catch (error) {
                  if (view.current === currentView) {
                    toastApiError(error, "Preview subscription change")
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
                !reason.trim() ||
                (Boolean(change.data) && change.data?.status !== "processing")
              }
              onClick={async () => {
                if (!request) return
                const currentView = view.current
                const changeKey =
                  attempts.current.get(requestKey) ?? crypto.randomUUID()
                attempts.current.set(requestKey, changeKey)
                const completeAttempt = () => {
                  if (attempts.current.get(requestKey) === changeKey) {
                    attempts.current.delete(requestKey)
                  }
                }
                try {
                  const result = await change.mutateAsync({
                    change: { ...request, reason: reason.trim() },
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
                      result.effective === "now"
                        ? "Subscription changed"
                        : "Change scheduled for the next renewal"
                    )
                    handleOpenChange(false)
                    setSelectedPriceId("")
                    setSeats("")
                    setReason("")
                    setReviewedKey("")
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
                        (error.code === "subscription_change_refused" ||
                          error.code === "subscription_change_idempotency_conflict")))
                  ) {
                    completeAttempt()
                  }
                  if (view.current === currentView) {
                    toastApiError(error, "Change subscription")
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
                  : "Confirm change"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
