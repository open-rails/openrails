import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import { HugeiconsIcon } from "@hugeicons/react"
import { Add01Icon, Delete02Icon, Edit02Icon } from "@hugeicons/core-free-icons"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import * as React from "react"

import { FormFieldErrors } from "#orck/admin/components/form-field-errors"
import { Button } from "#orck/admin/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "#orck/admin/components/ui/dialog"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "#orck/admin/components/ui/select"
import type { Meter } from "#orck/client/generated/wire"
import { DIALOG_FORM } from "#orck/admin/lib/dialog-width"

import {
  buildMeterRequest,
  meterDefinitionLocked,
  meterFormValues,
} from "./metering-model"

export function MeterFormDialog({
  meter,
  writesAllowed,
}: {
  meter?: Meter
  writesAllowed: boolean
}) {
  const { toast, toastApiError, adminMutations } = useAdminServices()

  const { id: adminId, text: adminText } = useAdminUI()

  const [open, setOpen] = React.useState(false)
  const [submitError, setSubmitError] = React.useState("")
  const queryClient = useQueryClient()
  const save = useMutation(adminMutations.putUsageMeter(queryClient))
  const locked = meter ? meterDefinitionLocked(meter, writesAllowed) : null
  const form = useForm({
    defaultValues: meterFormValues(meter),
    onSubmit: async ({ value }) => {
      setSubmitError("")
      try {
        const request = buildMeterRequest(value)
        // The revision this form read: a meter another edit changed since is
        // refused, and a new one must still be new.
        request.meter.expected_revision = meter?.revision ?? 0
        await save.mutateAsync(request)
        toast.success(meter ? "Meter updated" : "Meter created")
        setOpen(false)
        form.reset()
      } catch (error) {
        if (error instanceof Error && !("status" in error)) {
          setSubmitError(error.message)
          return
        }
        toastApiError(error, meter ? "Update meter" : "Create meter")
      }
    },
  })

  if (locked) {
    return (
      <Button variant="outline" size="sm" disabled title={locked}>
        <HugeiconsIcon icon={Edit02Icon} className="size-4" />
        <AdminMessage id={"Edit definition"} />
      </Button>
    )
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        setSubmitError("")
        if (!next) form.reset()
      }}
    >
      <DialogTrigger
        render={
          <Button variant={meter ? "outline" : "default"} size="sm">
            <HugeiconsIcon
              icon={meter ? Edit02Icon : Add01Icon}
              className="size-4"
            />
            {meter ? "Edit definition" : "Create meter"}
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <form
          className="grid gap-5"
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            void form.handleSubmit()
          }}
        >
          <DialogHeader>
            <DialogTitle>{meter ? "Edit meter" : "Create meter"}</DialogTitle>
            <DialogDescription>
              <AdminMessage
                id={
                  "Define the event fields OpenRails will aggregate. Your host still reports each event through the usage API."
                }
              />
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-4">
            <form.Field
              name="key"
              validators={{
                onBlur: ({ value }) =>
                  value.trim() ? undefined : "Enter a meter key.",
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor={adminId("meter-key")}>
                    <AdminMessage id={"Key"} />
                  </Label>
                  <Input
                    id={adminId("meter-key")}
                    value={field.state.value}
                    onChange={(event) => field.handleChange(event.target.value)}
                    onBlur={field.handleBlur}
                    disabled={Boolean(meter)}
                    placeholder={adminText("api-tokens")}
                    autoComplete="off"
                  />
                  <p className="text-xs text-muted-foreground">
                    <AdminMessage
                      id={
                        "Stable identifier. It cannot be renamed after creation."
                      }
                    />
                  </p>
                  <FormFieldErrors errors={field.state.meta.errors} />
                </div>
              )}
            </form.Field>

            <div className="grid gap-4 sm:grid-cols-2">
              <form.Field name="eventType">
                {(field) => (
                  <div className="grid gap-1.5">
                    <Label htmlFor={adminId("meter-event-type")}>
                      <AdminMessage id={"Event type"} />
                    </Label>
                    <Input
                      id={adminId("meter-event-type")}
                      value={field.state.value}
                      onChange={(event) =>
                        field.handleChange(event.target.value)
                      }
                      placeholder={adminText("token.used")}
                      autoComplete="off"
                    />
                    <p className="text-xs text-muted-foreground">
                      <AdminMessage id={"Leave blank to use the meter key."} />
                    </p>
                  </div>
                )}
              </form.Field>
              <form.Field name="unit">
                {(field) => (
                  <div className="grid gap-1.5">
                    <Label htmlFor={adminId("meter-unit")}>
                      <AdminMessage id={"Unit"} />
                    </Label>
                    <Input
                      id={adminId("meter-unit")}
                      value={field.state.value}
                      onChange={(event) =>
                        field.handleChange(event.target.value)
                      }
                      placeholder={adminText("tokens")}
                      autoComplete="off"
                    />
                    <p className="text-xs text-muted-foreground">
                      <AdminMessage
                        id={"Human-readable quantity shown in billing."}
                      />
                    </p>
                  </div>
                )}
              </form.Field>
            </div>

            <div className="grid gap-4 sm:grid-cols-2">
              <form.Field name="aggregation">
                {(field) => (
                  <div className="grid gap-1.5">
                    <Label htmlFor={adminId("meter-aggregation")}>
                      <AdminMessage id={"Aggregation"} />
                    </Label>
                    <Select
                      value={field.state.value}
                      onValueChange={(value) =>
                        field.handleChange(value as "sum" | "count")
                      }
                    >
                      <SelectTrigger
                        id={adminId("meter-aggregation")}
                        className="w-full"
                      >
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="sum">
                          <AdminMessage id={"Sum a numeric field"} />
                        </SelectItem>
                        <SelectItem value="count">
                          <AdminMessage id={"Count events"} />
                        </SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                )}
              </form.Field>
              <form.Subscribe selector={(state) => state.values.aggregation}>
                {(aggregation) =>
                  aggregation === "sum" ? (
                    <form.Field
                      name="valueProperty"
                      validators={{
                        onBlur: ({ value }) =>
                          value.trim()
                            ? undefined
                            : "Sum meters require a value property.",
                      }}
                    >
                      {(field) => (
                        <div className="grid gap-1.5">
                          <Label htmlFor={adminId("meter-value-property")}>
                            <AdminMessage id={"Value property"} />
                          </Label>
                          <Input
                            id={adminId("meter-value-property")}
                            value={field.state.value}
                            onChange={(event) =>
                              field.handleChange(event.target.value)
                            }
                            onBlur={field.handleBlur}
                            placeholder={adminText("tokens")}
                            autoComplete="off"
                          />
                          <FormFieldErrors errors={field.state.meta.errors} />
                        </div>
                      )}
                    </form.Field>
                  ) : (
                    <div className="self-end rounded-lg bg-muted/50 px-3 py-2 text-xs text-muted-foreground">
                      <AdminMessage
                        id={"Count uses one unit for every matching event."}
                      />
                    </div>
                  )
                }
              </form.Subscribe>
            </div>

            <form.Field name="groupBy" mode="array">
              {(field) => (
                <fieldset className="grid gap-2">
                  <div className="flex items-center justify-between gap-3">
                    <div>
                      <Label>
                        <AdminMessage id={"Dimensions"} />
                      </Label>
                      <p className="text-xs text-muted-foreground">
                        <AdminMessage
                          id={
                            "Register event properties used by filters or matrix rates."
                          }
                        />
                      </p>
                    </div>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      onClick={() => field.pushValue({ key: "", value: "" })}
                    >
                      <HugeiconsIcon icon={Add01Icon} className="size-4" />
                      <AdminMessage id={"Add dimension"} />
                    </Button>
                  </div>
                  {field.state.value.map((row, index) => (
                    <div
                      key={index}
                      className="grid grid-cols-[1fr_1fr_auto] gap-2"
                    >
                      <Input
                        aria-label={`Dimension ${index + 1} name`}
                        value={row.key}
                        onChange={(event) =>
                          form.setFieldValue(
                            `groupBy[${index}].key`,
                            event.target.value
                          )
                        }
                        placeholder={adminText("model")}
                        autoComplete="off"
                      />
                      <Input
                        aria-label={`Dimension ${index + 1} property`}
                        value={row.value}
                        onChange={(event) =>
                          form.setFieldValue(
                            `groupBy[${index}].value`,
                            event.target.value
                          )
                        }
                        placeholder={adminText("metadata.model")}
                        autoComplete="off"
                      />
                      <Button
                        type="button"
                        variant="ghost"
                        size="icon"
                        aria-label={`Remove dimension ${index + 1}`}
                        onClick={() => field.removeValue(index)}
                      >
                        <HugeiconsIcon icon={Delete02Icon} className="size-4" />
                      </Button>
                    </div>
                  ))}
                </fieldset>
              )}
            </form.Field>
          </div>

          {submitError && (
            <p role="alert" className="text-sm text-destructive">
              {submitError}
            </p>
          )}
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setOpen(false)}
            >
              <AdminMessage id={"Cancel"} />
            </Button>
            <form.Subscribe selector={(state) => state.isSubmitting}>
              {(submitting) => (
                <Button type="submit" disabled={submitting || save.isPending}>
                  {submitting || save.isPending ? "Saving…" : "Save meter"}
                </Button>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
