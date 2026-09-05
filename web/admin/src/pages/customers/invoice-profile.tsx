import { useState } from "react"
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { Link } from "react-router-dom"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { invoiceQueries, invoiceProfileMutation } from "@/lib/invoice-queries"
import type { InvoiceProfile } from "@/lib/api/invoice-types"
import {
  invoiceProfileValues,
  invoiceProfileRequest,
  type ProfileFormValues,
} from "../invoices/model"

export function CustomerInvoiceProfileSection({
  customerId,
}: {
  customerId: string
}) {
  const query = useQuery(invoiceQueries.profile(customerId))
  return (
    <Card>
      <CardHeader>
        <CardTitle>Invoice profile</CardTitle>
      </CardHeader>
      <CardContent>
        <Link
          className="text-sm underline"
          to={`/invoices?customer_id=${customerId}`}
        >
          View customer invoices
        </Link>
        {query.isPending ? (
          <p>Loading invoice profile…</p>
        ) : query.error ? (
          <p role="alert" className="text-destructive">
            {query.error.message}
          </p>
        ) : (
          query.data && (
            <InvoiceProfileEditor
              key={customerId}
              customerId={customerId}
              profile={query.data.profile}
              canUpdate={query.data.can_update}
            />
          )
        )}
      </CardContent>
    </Card>
  )
}
export function InvoiceProfileEditor({
  customerId,
  profile,
  canUpdate,
}: {
  customerId: string
  profile: InvoiceProfile | null
  canUpdate: boolean
}) {
  const [values, setValues] = useState(() => invoiceProfileValues(profile))
  const [error, setError] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)
  const client = useQueryClient(),
    mutation = useMutation(invoiceProfileMutation(client, customerId))
  const update = (patch: Partial<ProfileFormValues>) => {
    setValues((old) => ({ ...old, ...patch }))
    setSaved(false)
  }
  async function save() {
    setError(null)
    setSaved(false)
    try {
      await mutation.mutateAsync(invoiceProfileRequest(values, profile))
      setSaved(true)
    } catch (failure) {
      setError(
        failure instanceof Error
          ? failure.message
          : "Invoice profile update failed."
      )
    }
  }
  return (
    <form
      className="mt-4 space-y-4"
      onSubmit={(event) => {
        event.preventDefault()
        if (canUpdate && !mutation.isPending) void save()
      }}
    >
      <p className="text-sm text-muted-foreground">
        Terms and billing facts for future invoices. Issued invoices keep their
        original snapshots. Tax details are recorded; no tax is calculated.
      </p>
      <fieldset
        disabled={!canUpdate || mutation.isPending}
        className="space-y-4"
      >
        <div className="grid gap-3 sm:grid-cols-2">
          <div>
            <Label htmlFor="invoice-profile-terms">Payment terms (days)</Label>
            <Input
              id="invoice-profile-terms"
              type="number"
              min={0}
              step={1}
              value={values.terms}
              onChange={(e) => update({ terms: e.target.value })}
            />
          </div>
          <div>
            <Label htmlFor="invoice-profile-method">Collection method</Label>
            <select
              id="invoice-profile-method"
              className="h-9 w-full rounded-md border bg-background px-2"
              value={values.method}
              onChange={(e) =>
                update({
                  method: e.target.value as ProfileFormValues["method"],
                })
              }
            >
              <option value="charge_automatically">Saved payment method</option>
              <option value="send_invoice">Manual remittance</option>
            </select>
          </div>
        </div>
        <div>
          <Label htmlFor="invoice-profile-po">Purchase order</Label>
          <Input
            id="invoice-profile-po"
            value={values.po}
            onChange={(e) => update({ po: e.target.value })}
          />
        </div>
        <div>
          <Label htmlFor="invoice-profile-memo">Invoice memo</Label>
          <Input
            id="invoice-profile-memo"
            value={values.memo}
            onChange={(e) => update({ memo: e.target.value })}
          />
        </div>
        <div className="space-y-2">
          <p className="text-sm font-medium">Billing contacts</p>
          {values.contacts.map((contact, i) => (
            <div key={i} className="flex gap-2">
              <Input
                aria-label={`Contact ${i + 1} name`}
                placeholder="Name"
                value={contact.name}
                onChange={(e) =>
                  update({
                    contacts: values.contacts.map((c, index) =>
                      index === i ? { ...c, name: e.target.value } : c
                    ),
                  })
                }
              />
              <Input
                aria-label={`Contact ${i + 1} email`}
                type="email"
                placeholder="Email"
                value={contact.email}
                onChange={(e) =>
                  update({
                    contacts: values.contacts.map((c, index) =>
                      index === i ? { ...c, email: e.target.value } : c
                    ),
                  })
                }
              />
              <Button
                type="button"
                variant="outline"
                aria-label={`Remove contact ${i + 1}`}
                onClick={() =>
                  update({
                    contacts: values.contacts.filter((_, index) => index !== i),
                  })
                }
              >
                Remove
              </Button>
            </div>
          ))}
          {canUpdate && (
            <Button
              type="button"
              variant="outline"
              onClick={() =>
                update({
                  contacts: [...values.contacts, { name: "", email: "" }],
                })
              }
            >
              Add contact
            </Button>
          )}
        </div>
        <div className="space-y-2">
          <p className="text-sm font-medium">Tax details</p>
          {values.tax.map((row, i) => (
            <div key={i} className="flex gap-2">
              <Input
                aria-label={`Tax detail ${i + 1} name`}
                placeholder="Name, such as tax_id"
                value={row.key}
                onChange={(e) =>
                  update({
                    tax: values.tax.map((r, index) =>
                      index === i ? { ...r, key: e.target.value } : r
                    ),
                  })
                }
              />
              <Input
                aria-label={`Tax detail ${i + 1} value`}
                placeholder="Value"
                value={row.value}
                onChange={(e) =>
                  update({
                    tax: values.tax.map((r, index) =>
                      index === i ? { ...r, value: e.target.value } : r
                    ),
                  })
                }
              />
              <Button
                type="button"
                variant="outline"
                aria-label={`Remove tax detail ${i + 1}`}
                onClick={() =>
                  update({ tax: values.tax.filter((_, index) => index !== i) })
                }
              >
                Remove
              </Button>
            </div>
          ))}
          {canUpdate && (
            <Button
              type="button"
              variant="outline"
              onClick={() =>
                update({ tax: [...values.tax, { key: "", value: "" }] })
              }
            >
              Add tax detail
            </Button>
          )}
        </div>
      </fieldset>
      {error && (
        <p role="alert" className="text-destructive">
          {error}
        </p>
      )}
      {saved && (
        <p role="status" className="text-sm">
          Invoice profile saved. Issued invoices were not changed.
        </p>
      )}
      {canUpdate && (
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? "Saving…" : "Save invoice profile"}
        </Button>
      )}
    </form>
  )
}
