import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { useAuth } from "#orck/admin/lib/auth"

import type { CreditGrant } from "#orck/admin/lib/api/credit-types"
import {
  currencyScale,
  formatDate,
  formatNativeAmount,
  shortId,
} from "#orck/admin/lib/format"
import { CursorPager } from "#orck/admin/components/cursor-pager"
import { useCursorPages } from "#orck/admin/lib/cursor-pages"
import {
  canRevokeCredit,
  creditGrantInput,
  creditRevokeInput,
} from "./credit-form"
import { Button } from "#orck/admin/components/ui/button"
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "#orck/admin/components/ui/card"
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
import { useAdminUpdates } from "#orck/admin/lib/capabilities"

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
  const { id: adminId, text: adminText } = useAdminUI()

  const { creditQueries } = useAdminServices()

  const canUpdate = useAdminUpdates()
  const [currency, setCurrency] = React.useState(initialCurrency)
  const [currencyInput, setCurrencyInput] = React.useState(initialCurrency)
  const grantPages = useCursorPages(currency, 25)
  const ledgerPages = useCursorPages(currency, 25)
  const [granting, setGranting] = React.useState(false)
  const [revokeGrant, setRevokeGrant] = React.useState<CreditGrant | null>(null)
  const grants = useQuery(
    creditQueries.grants(
      merchant,
      customer,
      currency,
      grantPages.size,
      grantPages.cursor
    )
  )
  const ledger = useQuery(
    creditQueries.transactions(
      merchant,
      customer,
      currency,
      ledgerPages.size,
      ledgerPages.cursor
    )
  )
  const decimals = currencyScale(currency)

  return (
    <Card>
      <CardHeader className="gap-3">
        <div className="flex items-center justify-between gap-3">
          <CardTitle className="text-sm">
            <AdminMessage id={"Credits"} />
          </CardTitle>
          {canUpdate && (
            <Button
              size="sm"
              disabled={decimals === undefined}
              onClick={() => setGranting(true)}
            >
              <AdminMessage id={"Grant credit"} />
            </Button>
          )}
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
            <Label htmlFor={adminId("credit-currency")}>
              <AdminMessage id={"Currency"} />
            </Label>
            <Input
              id={adminId("credit-currency")}
              value={currencyInput}
              onChange={(e) => setCurrencyInput(e.target.value)}
              placeholder={adminText("USD")}
            />
          </div>
          <Button type="submit" variant="outline" size="sm">
            <AdminMessage id={"View"} />
          </Button>
        </form>
        {decimals === undefined && (
          <p className="text-xs text-muted-foreground">
            {currency}
            <AdminMessage id={" is not a supported currency."} />
          </p>
        )}
      </CardHeader>
      <CardContent className="grid gap-6">
        <section aria-label={adminText("Credit grants")}>
          <h3 className="mb-2 text-sm font-medium">
            <AdminMessage id={"Grants · "} />
            {currency}
          </h3>
          {grants.isPending ? (
            <p className="text-sm text-muted-foreground">
              <AdminMessage id={"Loading grants…"} />
            </p>
          ) : grants.error ? (
            <p role="alert" className="text-sm text-destructive">
              {errorText(grants.error)}
            </p>
          ) : !grants.data?.data.length ? (
            <p className="text-sm text-muted-foreground">
              <AdminMessage id={"No credit grants in this currency."} />
            </p>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>
                      <AdminMessage id={"Grant"} />
                    </TableHead>
                    <TableHead>
                      <AdminMessage id={"Original"} />
                    </TableHead>
                    <TableHead>
                      <AdminMessage id={"Remaining"} />
                    </TableHead>
                    <TableHead>
                      <AdminMessage id={"Spent"} />
                    </TableHead>
                    <TableHead>
                      <AdminMessage id={"Expired"} />
                    </TableHead>
                    <TableHead>
                      <AdminMessage id={"Revoked"} />
                    </TableHead>
                    <TableHead>
                      <AdminMessage id={"State"} />
                    </TableHead>
                    <TableHead>
                      <AdminMessage id={"Expires"} />
                    </TableHead>
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
                        {canUpdate && (
                          <Button
                            size="sm"
                            variant="outline"
                            disabled={!canRevokeCredit(grant)}
                            onClick={() => setRevokeGrant(grant)}
                          >
                            <AdminMessage id={"Revoke"} />
                          </Button>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
          <CursorPager
            pages={grantPages}
            rows={grants.data?.data?.length ?? 0}
            nextCursor={grants.data?.next_cursor}
            busy={grants.isFetching}
          />
        </section>
        <section aria-label={adminText("Credit transaction ledger")}>
          <h3 className="mb-2 text-sm font-medium">
            <AdminMessage id={"Transaction ledger · "} />
            {currency}
          </h3>
          {ledger.isPending ? (
            <p className="text-sm text-muted-foreground">
              <AdminMessage id={"Loading transactions…"} />
            </p>
          ) : ledger.error ? (
            <p role="alert" className="text-sm text-destructive">
              {errorText(ledger.error)}
            </p>
          ) : !ledger.data?.data.length ? (
            <p className="text-sm text-muted-foreground">
              <AdminMessage id={"No transactions in this currency."} />
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>
                    <AdminMessage id={"Date"} />
                  </TableHead>
                  <TableHead>
                    <AdminMessage id={"Type"} />
                  </TableHead>
                  <TableHead>
                    <AdminMessage id={"Amount"} />
                  </TableHead>
                  <TableHead>
                    <AdminMessage id={"Source"} />
                  </TableHead>
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
            rows={ledger.data?.data?.length ?? 0}
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
  const { toast, creditMutations } = useAdminServices()

  const { id: adminId } = useAdminUI()

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
          <DialogTitle>
            <AdminMessage id={"Grant credit"} />
          </DialogTitle>
          <DialogDescription>
            <AdminMessage id={"Add credit in "} />
            {currency}
            <AdminMessage id={" to this customer."} />
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
            <Label htmlFor={adminId("grant-amount")}>
              <AdminMessage id={"Amount ("} />
              {currency})
            </Label>
            <Input
              id={adminId("grant-amount")}
              inputMode="decimal"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              autoFocus
            />
          </div>
          <div className="grid gap-1">
            <Label htmlFor={adminId("grant-expiry")}>
              <AdminMessage id={"Expires (optional)"} />
            </Label>
            <Input
              id={adminId("grant-expiry")}
              type="datetime-local"
              value={expires}
              onChange={(e) => setExpires(e.target.value)}
            />
          </div>
          <div className="grid gap-1">
            <Label htmlFor={adminId("grant-description")}>
              <AdminMessage id={"Description (optional)"} />
            </Label>
            <Input
              id={adminId("grant-description")}
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
              <AdminMessage id={"Cancel"} />
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
  const { toast, creditMutations } = useAdminServices()

  const { id: adminId } = useAdminUI()

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
          <DialogTitle>
            <AdminMessage id={"Revoke remaining credit"} />
          </DialogTitle>
          <DialogDescription>
            <AdminMessage id={"Revoke the unspent remainder of"} />{" "}
            {formatNativeAmount(grant.remaining_amount, grant.currency)}
            <AdminMessage
              id={
                ". Active holds may prevent revocation. Payment refunds are managed separately."
              }
            />
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
            <Label htmlFor={adminId("revoke-reason")}>
              <AdminMessage id={"Reason"} />
            </Label>
            <Input
              id={adminId("revoke-reason")}
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
              <AdminMessage id={"Cancel"} />
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
