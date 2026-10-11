import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import { useState } from "react"
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { Link } from "#orck/admin/router"
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "#orck/admin/components/ui/card"
import { Button } from "#orck/admin/components/ui/button"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"

import type { InvoiceProfile } from "#orck/client/generated/wire"
import {
  invoiceProfileValues,
  invoiceProfileRequest,
  type ProfileFormValues,
} from "./invoice-profile-model"
import { useAdminUpdates } from "#orck/admin/lib/capabilities"

export function CustomerInvoiceProfileSection({
  customerId,
}: {
  customerId: string
}) {
  const { orderQueries } = useAdminServices()

  const canUpdate = useAdminUpdates()
  const query = useQuery(orderQueries.profile(customerId))
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <AdminMessage id={"Invoice profile"} />
        </CardTitle>
      </CardHeader>
      <CardContent>
        <Link
          className="text-sm underline"
          to={`/orders?customer_id=${customerId}&origin=usage`}
        >
          <AdminMessage id={"View customer usage orders"} />
        </Link>
        {query.isPending ? (
          <p>
            <AdminMessage id={"Loading invoice profile…"} />
          </p>
        ) : query.error ? (
          <p role="alert" className="text-destructive">
            {query.error.message}
          </p>
        ) : (
          <InvoiceProfileEditor
            key={customerId}
            customerId={customerId}
            profile={query.data}
            readOnly={!canUpdate}
          />
        )}
      </CardContent>
    </Card>
  )
}
// A customer without a profile edits an empty one; saving creates it.
export function InvoiceProfileEditor({
  customerId,
  profile,
  readOnly = false,
}: {
  customerId: string
  profile: InvoiceProfile | null
  // readOnly shows the profile to staff who may not change it.
  readOnly?: boolean
}) {
  const { id: adminId, text: adminText } = useAdminUI()

  const { invoiceProfileMutation } = useAdminServices()

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
        if (!mutation.isPending) void save()
      }}
    >
      <p className="text-sm text-muted-foreground">
        <AdminMessage
          id={
            "Terms and billing facts for future usage orders. Billed orders keep their original snapshots. Tax details are recorded; no tax is calculated."
          }
        />
      </p>
      <fieldset disabled={readOnly || mutation.isPending} className="space-y-4">
        <div className="grid gap-3 sm:grid-cols-2">
          <div>
            <Label htmlFor={adminId("invoice-profile-terms")}>
              <AdminMessage id={"Payment terms (days)"} />
            </Label>
            <Input
              id={adminId("invoice-profile-terms")}
              type="number"
              min={0}
              step={1}
              value={values.terms}
              onChange={(e) => update({ terms: e.target.value })}
            />
          </div>
          <div>
            <Label htmlFor={adminId("invoice-profile-method")}>
              <AdminMessage id={"Collection method"} />
            </Label>
            <select
              id={adminId("invoice-profile-method")}
              className="h-9 w-full rounded-md border bg-background px-2"
              value={values.method}
              onChange={(e) =>
                update({
                  method: e.target.value as ProfileFormValues["method"],
                })
              }
            >
              <option value="charge_automatically">
                <AdminMessage id={"Saved payment method"} />
              </option>
              <option value="send_invoice">
                <AdminMessage id={"Manual remittance"} />
              </option>
            </select>
          </div>
        </div>
        <div>
          <Label htmlFor={adminId("invoice-profile-po")}>
            <AdminMessage id={"Purchase order"} />
          </Label>
          <Input
            id={adminId("invoice-profile-po")}
            value={values.po}
            onChange={(e) => update({ po: e.target.value })}
          />
        </div>
        <div>
          <Label htmlFor={adminId("invoice-profile-memo")}>
            <AdminMessage id={"Invoice memo"} />
          </Label>
          <Input
            id={adminId("invoice-profile-memo")}
            value={values.memo}
            onChange={(e) => update({ memo: e.target.value })}
          />
        </div>
        <div className="space-y-2">
          <p className="text-sm font-medium">
            <AdminMessage id={"Billing contacts"} />
          </p>
          {values.contacts.map((contact, i) => (
            <div key={i} className="flex gap-2">
              <Input
                aria-label={`Contact ${i + 1} name`}
                placeholder={adminText("Name")}
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
                placeholder={adminText("Email")}
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
                <AdminMessage id={"Remove"} />
              </Button>
            </div>
          ))}
          <Button
            type="button"
            variant="outline"
            onClick={() =>
              update({
                contacts: [...values.contacts, { name: "", email: "" }],
              })
            }
          >
            <AdminMessage id={"Add contact"} />
          </Button>
        </div>
        <div className="space-y-2">
          <p className="text-sm font-medium">
            <AdminMessage id={"Tax details"} />
          </p>
          {values.tax.map((row, i) => (
            <div key={i} className="flex gap-2">
              <Input
                aria-label={`Tax detail ${i + 1} name`}
                placeholder={adminText("Name, such as tax_id")}
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
                placeholder={adminText("Value")}
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
                <AdminMessage id={"Remove"} />
              </Button>
            </div>
          ))}
          <Button
            type="button"
            variant="outline"
            onClick={() =>
              update({ tax: [...values.tax, { key: "", value: "" }] })
            }
          >
            <AdminMessage id={"Add tax detail"} />
          </Button>
        </div>
      </fieldset>
      {error && (
        <p role="alert" className="text-destructive">
          {error}
        </p>
      )}
      {saved && (
        <p role="status" className="text-sm">
          <AdminMessage
            id={"Invoice profile saved. Billed orders were not changed."}
          />
        </p>
      )}
      {!readOnly && (
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? "Saving…" : "Save invoice profile"}
        </Button>
      )}
    </form>
  )
}
