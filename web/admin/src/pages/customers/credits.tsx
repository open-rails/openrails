import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { useAuth } from "@/lib/auth"
import { creditMutations, creditQueries } from "@/lib/credit-queries"
import type { CreditGrant } from "@/lib/api/credit-types"
import {
  currencyScale,
  formatDate,
  formatNativeAmount,
  shortId,
} from "@/lib/format"
import { CursorPager } from "@/components/cursor-pager"
import { useCursorPages } from "@/lib/cursor-pages"
import {
  canRevokeCredit,
  creditGrantInput,
  creditRevokeInput,
} from "./credit-form"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"

const PAGE = 20
const errorText = (error: unknown) =>
  error instanceof Error ? error.message : "The request failed. Try again."

export function CustomerCreditSupportSection({
  customerId,
  currencies,
}: {
  customerId: string
  currencies: string[]
}) {
  const { activeMerchant } = useAuth()
  const merchant = activeMerchant?.slug ?? ""
  return (
    <CreditSupport
      key={`${merchant}:${customerId}`}
      merchant={merchant}
      customer={customerId}
      initialCurrency={currencies[0] ?? "USD"}
    />
  )
}

function CreditSupport({
  merchant,
  customer,
  initialCurrency,
}: {
  merchant: string
  customer: string
  initialCurrency: string
}) {
  const [currency, setCurrency] = React.useState(initialCurrency)
  const [currencyInput, setCurrencyInput] = React.useState(initialCurrency)
  const grantPages = useCursorPages(currency)
  const ledgerPages = useCursorPages(currency)
  const [granting, setGranting] = React.useState(false)
  const [revokeGrant, setRevokeGrant] = React.useState<CreditGrant | null>(null)
  const grants = useQuery(
    creditQueries.grants(merchant, customer, currency, PAGE, grantPages.cursor)
  )
  const ledger = useQuery(
    creditQueries.transactions(
      merchant,
      customer,
      currency,
      PAGE,
      ledgerPages.cursor
    )
  )
  const decimals = currencyScale(currency)

  return (
    <Card>
      <CardHeader className="gap-3">
        <div className="flex items-center justify-between gap-3">
          <CardTitle className="text-sm">Credits</CardTitle>
          <Button
            size="sm"
            disabled={decimals === undefined}
            onClick={() => setGranting(true)}
          >
            Grant credit
          </Button>
        </div>
        <form
          className="flex max-w-sm items-end gap-2"
          onSubmit={(event) => {
            event.preventDefault()
            const value = currencyInput.trim()
            if (value) setCurrency(value.toUpperCase())
          }}
        >
          <div className="grid gap-1">
            <Label htmlFor="credit-currency">Currency</Label>
            <Input
              id="credit-currency"
              value={currencyInput}
              onChange={(e) => setCurrencyInput(e.target.value)}
              placeholder="USD"
            />
          </div>
          <Button type="submit" variant="outline" size="sm">
            View
          </Button>
        </form>
        {decimals === undefined && (
          <p className="text-xs text-muted-foreground">
            {currency} is not a supported currency.
          </p>
        )}
      </CardHeader>
      <CardContent className="grid gap-6">
        <section aria-label="Credit grants">
          <h3 className="mb-2 text-sm font-medium">Grants · {currency}</h3>
          {grants.isPending ? (
            <p className="text-sm text-muted-foreground">Loading grants…</p>
          ) : grants.error ? (
            <p role="alert" className="text-sm text-destructive">
              {errorText(grants.error)}
            </p>
          ) : !grants.data?.data.length ? (
            <p className="text-sm text-muted-foreground">
              No credit grants in this currency.
            </p>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Grant</TableHead>
                    <TableHead>Original</TableHead>
                    <TableHead>Remaining</TableHead>
                    <TableHead>Spent</TableHead>
                    <TableHead>Expired</TableHead>
                    <TableHead>Revoked</TableHead>
                    <TableHead>State</TableHead>
                    <TableHead>Expires</TableHead>
                    <TableHead />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {grants.data.data.map((grant) => (
                    <TableRow key={grant.id}>
                      <TableCell>
                        <span title={grant.id}>{shortId(grant.id)}</span>
                        <div className="text-xs text-muted-foreground">
                          {formatDate(grant.created_at)}
                        </div>
                        <div className="text-xs text-muted-foreground">
                          {grant.description ?? grant.source_type}
                        </div>
                      </TableCell>
                      {[
                        grant.amount,
                        grant.remaining_amount,
                        grant.spent_amount,
                        grant.expired_amount,
                        grant.revoked_amount,
                      ].map((amount, index) => (
                        <TableCell key={index} className="tabular-nums">
                          {formatNativeAmount(amount, grant.currency)}
                        </TableCell>
                      ))}
                      <TableCell>{grant.state}</TableCell>
                      <TableCell>
                        {grant.expires_at
                          ? formatDate(grant.expires_at)
                          : "No expiry"}
                      </TableCell>
                      <TableCell>
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={!canRevokeCredit(grant)}
                          onClick={() => setRevokeGrant(grant)}
                        >
                          Revoke
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
          <CursorPager
            pages={grantPages}
            nextCursor={grants.data?.next_cursor}
            busy={grants.isFetching}
          />
        </section>
        <section aria-label="Credit transaction ledger">
          <h3 className="mb-2 text-sm font-medium">
            Transaction ledger · {currency}
          </h3>
          {ledger.isPending ? (
            <p className="text-sm text-muted-foreground">
              Loading transactions…
            </p>
          ) : ledger.error ? (
            <p role="alert" className="text-sm text-destructive">
              {errorText(ledger.error)}
            </p>
          ) : !ledger.data?.data.length ? (
            <p className="text-sm text-muted-foreground">
              No transactions in this currency.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Date</TableHead>
                  <TableHead>Type</TableHead>
                  <TableHead>Amount</TableHead>
                  <TableHead>Source</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {ledger.data.data.map((tx) => (
                  <TableRow key={tx.id}>
                    <TableCell title={tx.id}>
                      {formatDate(tx.created_at)}
                    </TableCell>
                    <TableCell>{tx.type.replaceAll("_", " ")}</TableCell>
                    <TableCell className="tabular-nums">
                      {formatNativeAmount(tx.amount, tx.currency)}
                    </TableCell>
                    <TableCell>{tx.source || "—"}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
          <CursorPager
            pages={ledgerPages}
            nextCursor={ledger.data?.next_cursor}
            busy={ledger.isFetching}
          />
        </section>
      </CardContent>
      {granting && decimals !== undefined && (
        <GrantCreditDialog
          merchant={merchant}
          customer={customer}
          currency={currency}
          decimals={decimals}
          onClose={() => setGranting(false)}
        />
      )}
      {revokeGrant && (
        <RevokeCreditDialog
          merchant={merchant}
          customer={customer}
          grant={revokeGrant}
          onClose={() => setRevokeGrant(null)}
        />
      )}
    </Card>
  )
}

function GrantCreditDialog({
  merchant,
  customer,
  currency,
  decimals,
  onClose,
}: {
  merchant: string
  customer: string
  currency: string
  decimals: number
  onClose: () => void
}) {
  const client = useQueryClient()
  const mutation = useMutation(
    creditMutations.grant(client, merchant, customer)
  )
  const [amount, setAmount] = React.useState("")
  const [expires, setExpires] = React.useState("")
  const [description, setDescription] = React.useState("")
  const [sourceID] = React.useState(() => crypto.randomUUID())
  const [error, setError] = React.useState("")
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !mutation.isPending) onClose()
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Grant credit</DialogTitle>
          <DialogDescription>
            Add credit in {currency} to this customer.
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-4"
          onSubmit={async (e) => {
            e.preventDefault()
            setError("")
            try {
              const body = creditGrantInput({
                amount,
                currency,
                decimals,
                expires,
                description,
                sourceID,
              })
              await mutation.mutateAsync(body)
              toast.success("Credit granted")
              onClose()
            } catch (error) {
              setError(errorText(error))
            }
          }}
        >
          <div className="grid gap-1">
            <Label htmlFor="grant-amount">Amount ({currency})</Label>
            <Input
              id="grant-amount"
              inputMode="decimal"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              autoFocus
            />
          </div>
          <div className="grid gap-1">
            <Label htmlFor="grant-expiry">Expires (optional)</Label>
            <Input
              id="grant-expiry"
              type="datetime-local"
              value={expires}
              onChange={(e) => setExpires(e.target.value)}
            />
          </div>
          <div className="grid gap-1">
            <Label htmlFor="grant-description">Description (optional)</Label>
            <Input
              id="grant-description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              maxLength={500}
            />
          </div>
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={mutation.isPending}
              onClick={onClose}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending ? "Granting…" : "Grant credit"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function RevokeCreditDialog({
  merchant,
  customer,
  grant,
  onClose,
}: {
  merchant: string
  customer: string
  grant: CreditGrant
  onClose: () => void
}) {
  const client = useQueryClient()
  const mutation = useMutation(
    creditMutations.revoke(client, merchant, customer)
  )
  const [reason, setReason] = React.useState("")
  const [error, setError] = React.useState("")
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !mutation.isPending) onClose()
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Revoke remaining credit</DialogTitle>
          <DialogDescription>
            Revoke the unspent remainder of{" "}
            {formatNativeAmount(grant.remaining_amount, grant.currency)}. Active
            holds may prevent revocation. Payment refunds are managed
            separately.
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-4"
          onSubmit={async (e) => {
            e.preventDefault()
            setError("")
            try {
              const result = await mutation.mutateAsync(
                creditRevokeInput(grant, reason)
              )
              toast.success(
                result.replayed
                  ? "Credit was already revoked"
                  : `${formatNativeAmount(result.revoked_amount, result.currency)} revoked`
              )
              onClose()
            } catch (error) {
              setError(errorText(error))
            }
          }}
        >
          <div className="grid gap-1">
            <Label htmlFor="revoke-reason">Reason</Label>
            <Input
              id="revoke-reason"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              maxLength={500}
              autoFocus
            />
          </div>
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={mutation.isPending}
              onClick={onClose}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              variant="destructive"
              disabled={!canRevokeCredit(grant) || mutation.isPending}
            >
              {mutation.isPending ? "Revoking…" : "Revoke credit"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
