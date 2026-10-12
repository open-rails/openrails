import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import { HugeiconsIcon } from "@hugeicons/react"
import {
  Add01Icon,
  Delete02Icon,
  Edit02Icon,
  Money03Icon,
} from "@hugeicons/core-free-icons"
import {
  useForm,
  type FormAsyncValidateOrFn,
  type FormValidateOrFn,
  type ReactFormExtendedApi,
} from "@tanstack/react-form"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import * as React from "react"
import { Link } from "#orck/admin/router"

import { useConfirm } from "#orck/admin/lib/confirm"
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
import { Switch } from "#orck/admin/components/ui/switch"
import type { Meter, Product } from "#orck/client/generated/wire"
import type { DefaultUsageRateCardRequest } from "#orck/admin/lib/api/endpoints"
import { DIALOG_WIDE } from "#orck/admin/lib/dialog-width"

import {
  buildRateCardRequest,
  RateCardFormError,
  rateCardFormValues,
  summarizeRateCard,
} from "./metering-model"

interface FormIssue {
  fieldId: string
  message: string
}

const RateCardIssueContext = React.createContext<FormIssue | null>(null)

export function RateCardEditor({
  meter,
  writesAllowed = false,
  products,
  productsPending = false,
  productsError = false,
  productsForbidden = false,
}: {
  meter: Meter
  writesAllowed?: boolean
  products: Product[]
  productsPending?: boolean
  productsError?: boolean
  productsForbidden?: boolean
}) {
  const { toast, toastApiError, adminMutations } = useAdminServices()

  const { id: adminId, text: adminText } = useAdminUI()

  const [open, setOpen] = React.useState(false)
  const [submitError, setSubmitError] = React.useState<FormIssue | null>(null)
  const confirm = useConfirm()
  const queryClient = useQueryClient()
  const saveDefault = useMutation(
    adminMutations.putDefaultUsageRateCard(queryClient)
  )
  const activeProducts = products.filter((product) => !product.archived)
  const existing = meter.rate_card
  const savePending = saveDefault.isPending
  const form = useForm({
    defaultValues: rateCardFormValues(existing),
    onSubmit: async ({ value }) => {
      setSubmitError(null)
      try {
        const rateCard = buildRateCardRequest(value)
        if (
          existing &&
          !(await confirm({
            title: "Replace the default rate?",
            description:
              "Billed usage orders and amounts already accrued do not change. The new rate can only affect the remaining positive delta in the current unfinalized period and future periods.",
            confirmLabel: "Replace rate",
            cancelLabel: "Keep current rate",
          }))
        )
          return
        await persist(rateCard)
      } catch (error) {
        if (error instanceof RateCardFormError) {
          setSubmitError({
            fieldId: error.fieldId,
            message: error.message,
          })
          requestAnimationFrame(() => {
            document.getElementById(adminId(error.fieldId))?.focus()
          })
          return
        }
        toastApiError(error, "Save rate card")
      }
    },
  })

  const persist = async (rateCard: DefaultUsageRateCardRequest) => {
    try {
      await saveDefault.mutateAsync({ meter, rateCard })
      toast.success(existing ? "Default rate replaced" : "Default rate added")
      setOpen(false)
    } catch (error) {
      toastApiError(error, "Save default rate")
    }
  }

  if (!writesAllowed || !meter.billing_supported) {
    return null
  }

  return (
    <RateCardIssueContext.Provider value={submitError}>
      <Dialog
        open={open}
        onOpenChange={(next) => {
          setOpen(next)
          setSubmitError(null)
          if (!next) form.reset()
        }}
      >
        <DialogTrigger
          render={
            <Button variant={existing ? "outline" : "default"} size="sm">
              <HugeiconsIcon
                icon={existing ? Edit02Icon : Money03Icon}
                className="size-4"
              />
              {existing ? "Edit default rate" : "Add default rate"}
            </Button>
          }
        />
        <DialogContent
          className={`${DIALOG_WIDE} max-h-[calc(100dvh-2rem)] overflow-y-auto`}
        >
          <form
            className="grid gap-6"
            onChange={() => setSubmitError(null)}
            onSubmit={(event) => {
              event.preventDefault()
              event.stopPropagation()
              void form.handleSubmit()
            }}
          >
            <DialogHeader>
              <DialogTitle>
                {existing ? "Edit default rate" : "Add default rate"}
              </DialogTitle>
              <DialogDescription>
                <AdminMessage
                  id={
                    "Set the price that charges every customer's usage as it is recorded. Money is entered in major currency units."
                  }
                />
              </DialogDescription>
            </DialogHeader>

            <section className="grid gap-4">
              <h3 className="text-sm font-medium">
                <AdminMessage id={"Product and model"} />
              </h3>
              {productsPending ? (
                <div className="rounded-lg bg-muted/50 p-3 text-sm text-muted-foreground">
                  <AdminMessage id={"Loading products…"} />
                </div>
              ) : productsError ? (
                <div
                  role="alert"
                  className="rounded-lg bg-muted/50 p-3 text-sm text-destructive"
                >
                  {productsForbidden
                    ? "Your role cannot read products for this merchant."
                    : "Products could not be loaded. Close this form and try again."}
                </div>
              ) : activeProducts.length === 0 ? (
                <div className="rounded-lg bg-muted/50 p-3 text-sm">
                  <p>
                    <AdminMessage id={"No active products are available."} />
                  </p>
                  <Link
                    className="text-primary underline underline-offset-3"
                    to="/catalog"
                  >
                    <AdminMessage id={"Create or activate a product first"} />
                  </Link>
                </div>
              ) : (
                <div className="grid gap-4 sm:grid-cols-3">
                  <form.Field name="productId">
                    {(field) => (
                      <div className="grid gap-1.5 sm:col-span-2">
                        <Label htmlFor={adminId("rate-product")}>
                          <AdminMessage id={"Product"} />
                        </Label>
                        <Select
                          value={field.state.value || null}
                          onValueChange={(value) =>
                            field.handleChange(value ?? "")
                          }
                        >
                          <SelectTrigger
                            id={adminId("rate-product")}
                            className="w-full"
                            {...fieldIssueProps("rate-product", submitError)}
                          >
                            <SelectValue
                              placeholder={adminText("Select product")}
                            />
                          </SelectTrigger>
                          <SelectContent>
                            {activeProducts.map((product) => (
                              <SelectItem key={product.id} value={product.id}>
                                {product.display_name}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                        <FieldIssue fieldId="rate-product" />
                      </div>
                    )}
                  </form.Field>
                  <form.Field name="currency">
                    {(field) => (
                      <div className="grid gap-1.5">
                        <Label htmlFor={adminId("rate-currency")}>
                          <AdminMessage id={"Currency"} />
                        </Label>
                        <Input
                          id={adminId("rate-currency")}
                          value={field.state.value}
                          onChange={(event) =>
                            field.handleChange(event.target.value.toUpperCase())
                          }
                          maxLength={3}
                          placeholder={adminText("USD")}
                          className="uppercase"
                          autoComplete="off"
                          {...fieldIssueProps("rate-currency", submitError)}
                        />
                        <FieldIssue fieldId="rate-currency" />
                      </div>
                    )}
                  </form.Field>
                  <PricingModelField form={form} />
                </div>
              )}
            </section>

            <form.Subscribe selector={(state) => state.values.model}>
              {(model) => (
                <section className="grid gap-4 border-t pt-5">
                  <h3 className="text-sm font-medium">
                    <AdminMessage id={"Rate"} />
                  </h3>
                  {model === "per_unit" && (
                    <PerUnitFields form={form} meter={meter} />
                  )}
                  {model === "tiered" && <TieredFields form={form} />}
                  {model === "package" && <PackageFields form={form} />}
                </section>
              )}
            </form.Subscribe>

            <FilterFields form={form} meter={meter} />
            <AllowanceFields form={form} />

            <form.Subscribe selector={(state) => state.values}>
              {(values) => <RateSummary values={values} />}
            </form.Subscribe>

            {submitError && (
              <p role="alert" className="text-sm text-destructive">
                {submitError.message}
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
              <Button
                type="submit"
                disabled={
                  savePending ||
                  productsPending ||
                  productsError ||
                  activeProducts.length === 0
                }
              >
                {savePending
                  ? "Saving…"
                  : existing
                    ? "Review replacement"
                    : "Save default rate"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </RateCardIssueContext.Provider>
  )
}

type RateCardFormValuesCompat = ReturnType<typeof rateCardFormValues>
type RateCardValidator = FormValidateOrFn<RateCardFormValuesCompat> | undefined
type RateCardAsyncValidator =
  FormAsyncValidateOrFn<RateCardFormValuesCompat> | undefined
type MeteringForm = ReactFormExtendedApi<
  RateCardFormValuesCompat,
  RateCardValidator,
  RateCardValidator,
  RateCardAsyncValidator,
  RateCardValidator,
  RateCardAsyncValidator,
  RateCardValidator,
  RateCardAsyncValidator,
  RateCardValidator,
  RateCardAsyncValidator,
  RateCardAsyncValidator,
  unknown
>

function PricingModelField({ form }: { form: MeteringForm }) {
  const { id: adminId } = useAdminUI()

  return (
    <form.Field name="model">
      {(field) => (
        <div className="grid gap-1.5 sm:col-span-3">
          <Label htmlFor={adminId("rate-model")}>
            <AdminMessage id={"Pricing model"} />
          </Label>
          <Select
            value={field.state.value}
            onValueChange={(value) =>
              field.handleChange(value as "per_unit" | "tiered" | "package")
            }
          >
            <SelectTrigger id={adminId("rate-model")} className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="per_unit">
                <AdminMessage id={"Per unit"} />
              </SelectItem>
              <SelectItem value="tiered">
                <AdminMessage id={"Tiered"} />
              </SelectItem>
              <SelectItem value="package">
                <AdminMessage id={"Package"} />
              </SelectItem>
            </SelectContent>
          </Select>
        </div>
      )}
    </form.Field>
  )
}

function PerUnitFields({ form, meter }: { form: MeteringForm; meter: Meter }) {
  const { id: adminId, text: adminText } = useAdminUI()

  return (
    <>
      <form.Field name="matrixEnabled">
        {(field) => (
          <div className="flex items-center justify-between gap-4 rounded-lg bg-muted/50 px-3 py-2.5">
            <div>
              <Label htmlFor={adminId("rate-matrix")}>
                <AdminMessage id={"Use matrix rates"} />
              </Label>
              <p className="text-xs text-muted-foreground">
                <AdminMessage
                  id={
                    "Price each value of one registered dimension separately."
                  }
                />
              </p>
            </div>
            <Switch
              id={adminId("rate-matrix")}
              checked={field.state.value}
              onCheckedChange={field.handleChange}
              disabled={Object.keys(meter.group_by ?? {}).length === 0}
            />
          </div>
        )}
      </form.Field>
      <form.Subscribe selector={(state) => state.values.matrixEnabled}>
        {(matrixEnabled) =>
          matrixEnabled ? (
            <MatrixFields form={form} meter={meter} />
          ) : (
            <form.Field name="unitAmount">
              {(field) => (
                <MoneyField
                  id={"rate-unit-amount"}
                  label={adminText("Price per divisor")}
                  value={field.state.value}
                  onChange={field.handleChange}
                  placeholder="0.0025"
                />
              )}
            </form.Field>
          )
        }
      </form.Subscribe>
      <div className="grid gap-4 sm:grid-cols-3">
        <form.Field name="divideBy">
          {(field) => (
            <TextField
              id={"rate-divide-by"}
              label={adminText("Divisor")}
              value={field.state.value}
              onChange={field.handleChange}
              inputMode="numeric"
              placeholder="1"
            />
          )}
        </form.Field>
        <form.Field name="round">
          {(field) => (
            <div className="grid gap-1.5">
              <Label htmlFor={adminId("rate-round")}>
                <AdminMessage id={"Rounding"} />
              </Label>
              <Select
                value={field.state.value}
                onValueChange={(value) =>
                  field.handleChange(value as "half_up" | "up" | "down")
                }
              >
                <SelectTrigger id={adminId("rate-round")} className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="half_up">
                    <AdminMessage id={"Nearest"} />
                  </SelectItem>
                  <SelectItem value="up">
                    <AdminMessage id={"Up"} />
                  </SelectItem>
                  <SelectItem value="down">
                    <AdminMessage id={"Down"} />
                  </SelectItem>
                </SelectContent>
              </Select>
            </div>
          )}
        </form.Field>
        <form.Field name="maximumAmount">
          {(field) => (
            <MoneyField
              id={"rate-maximum"}
              label={adminText("Period maximum")}
              value={field.state.value}
              onChange={field.handleChange}
              placeholder={adminText("No maximum")}
            />
          )}
        </form.Field>
      </div>
    </>
  )
}

function MatrixFields({ form, meter }: { form: MeteringForm; meter: Meter }) {
  const { id: adminId, text: adminText } = useAdminUI()

  const formIssue = React.useContext(RateCardIssueContext)
  return (
    <>
      <form.Field name="matrixDimension">
        {(field) => (
          <div className="grid gap-1.5">
            <Label htmlFor={adminId("matrix-dimension")}>
              <AdminMessage id={"Dimension"} />
            </Label>
            <Select
              value={field.state.value || null}
              onValueChange={(value) => field.handleChange(value ?? "")}
            >
              <SelectTrigger
                id={adminId("matrix-dimension")}
                className="w-full"
                {...fieldIssueProps("matrix-dimension", formIssue)}
              >
                <SelectValue placeholder={adminText("Select dimension")} />
              </SelectTrigger>
              <SelectContent>
                {Object.keys(meter.group_by ?? {}).map((key) => (
                  <SelectItem key={key} value={key}>
                    {key}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <FieldIssue fieldId="matrix-dimension" />
          </div>
        )}
      </form.Field>
      <form.Field name="matrixCells" mode="array">
        {(field) => (
          <div className="grid gap-2">
            <div className="flex items-center justify-between gap-3">
              <Label>
                <AdminMessage id={"Matrix cells"} />
              </Label>
              <Button
                id={adminId("matrix-add-cell")}
                type="button"
                variant="outline"
                size="sm"
                onClick={() =>
                  field.pushValue({
                    key: "",
                    unitAmount: "",
                    maximumAmount: "",
                  })
                }
              >
                <HugeiconsIcon icon={Add01Icon} className="size-4" />
                <AdminMessage id={"Add cell"} />
              </Button>
            </div>
            <FieldIssue fieldId="matrix-add-cell" />
            {field.state.value.length > 0 && (
              <div className="hidden gap-2 px-0.5 text-xs text-muted-foreground sm:grid sm:grid-cols-[1fr_1fr_1fr_auto]">
                <span>
                  <AdminMessage id="Value" />
                </span>
                <span>
                  <AdminMessage id="Unit price" />
                </span>
                <span>
                  <AdminMessage id="Maximum" />
                </span>
                <span className="size-8" aria-hidden="true" />
              </div>
            )}
            {field.state.value.map((row, index) => (
              <div
                key={index}
                className="grid gap-2 sm:grid-cols-[1fr_1fr_1fr_auto]"
              >
                <RowTextField
                  id={`matrix-cell-${index}-value`}
                  label={adminText("Value")}
                  value={row.key}
                  onChange={(value) =>
                    form.setFieldValue(`matrixCells[${index}].key`, value)
                  }
                  placeholder={adminText("gpt-5")}
                />
                <RowTextField
                  id={`matrix-cell-${index}-unit-price`}
                  label={adminText("Unit price")}
                  value={row.unitAmount}
                  onChange={(value) =>
                    form.setFieldValue(
                      `matrixCells[${index}].unitAmount`,
                      value
                    )
                  }
                  inputMode="decimal"
                  placeholder={adminText("Price")}
                />
                <RowTextField
                  id={`matrix-cell-${index}-maximum`}
                  label={adminText("Maximum")}
                  value={row.maximumAmount}
                  onChange={(value) =>
                    form.setFieldValue(
                      `matrixCells[${index}].maximumAmount`,
                      value
                    )
                  }
                  inputMode="decimal"
                  placeholder={adminText("Maximum")}
                />
                <RemoveRowButton
                  label={`Remove matrix cell ${index + 1}`}
                  onClick={() => field.removeValue(index)}
                />
              </div>
            ))}
          </div>
        )}
      </form.Field>
    </>
  )
}

function TieredFields({ form }: { form: MeteringForm }) {
  const { id: adminId, text: adminText } = useAdminUI()

  return (
    <>
      <form.Field name="tiers" mode="array">
        {(field) => (
          <div className="grid gap-2">
            <div className="flex items-center justify-between gap-3">
              <div>
                <Label>
                  <AdminMessage id={"Tiers"} />
                </Label>
                <p className="text-xs text-muted-foreground">
                  <AdminMessage
                    id={"Leave the final limit blank for the unbounded tier."}
                  />
                </p>
              </div>
              <Button
                id={adminId("tier-add")}
                type="button"
                variant="outline"
                size="sm"
                onClick={() =>
                  field.pushValue({ upTo: "", unitAmount: "", flatAmount: "" })
                }
              >
                <HugeiconsIcon icon={Add01Icon} className="size-4" />
                <AdminMessage id={"Add tier"} />
              </Button>
            </div>
            <FieldIssue fieldId="tier-add" />
            {field.state.value.length > 0 && (
              <div className="hidden gap-2 px-0.5 text-xs text-muted-foreground sm:grid sm:grid-cols-[1fr_1fr_1fr_auto]">
                <span>
                  <AdminMessage id={"Upper limit"} />
                </span>
                <span>
                  <AdminMessage id={"Unit price"} />
                </span>
                <span>
                  <AdminMessage id={"Flat amount"} />
                </span>
                <span className="size-8" aria-hidden="true" />
              </div>
            )}
            {field.state.value.map((row, index) => (
              <div
                key={index}
                className="grid gap-2 sm:grid-cols-[1fr_1fr_1fr_auto]"
              >
                <RowTextField
                  id={`tier-${index}-limit`}
                  label={adminText("Upper limit")}
                  value={row.upTo}
                  onChange={(value) =>
                    form.setFieldValue(`tiers[${index}].upTo`, value)
                  }
                  inputMode="numeric"
                  placeholder={
                    index === field.state.value.length - 1
                      ? "No limit"
                      : "Up to"
                  }
                />
                <RowTextField
                  id={`tier-${index}-unit-price`}
                  label={adminText("Unit price")}
                  value={row.unitAmount}
                  onChange={(value) =>
                    form.setFieldValue(`tiers[${index}].unitAmount`, value)
                  }
                  inputMode="decimal"
                  placeholder={adminText("Unit price")}
                />
                <RowTextField
                  id={`tier-${index}-flat-amount`}
                  label={adminText("Flat amount")}
                  value={row.flatAmount}
                  onChange={(value) =>
                    form.setFieldValue(`tiers[${index}].flatAmount`, value)
                  }
                  inputMode="decimal"
                  placeholder={adminText("Flat amount")}
                />
                <RemoveRowButton
                  label={`Remove tier ${index + 1}`}
                  onClick={() => field.removeValue(index)}
                  disabled={field.state.value.length === 1}
                />
              </div>
            ))}
          </div>
        )}
      </form.Field>
    </>
  )
}

function PackageFields({ form }: { form: MeteringForm }) {
  const { text: adminText } = useAdminUI()

  return (
    <div className="grid gap-4 sm:grid-cols-3">
      <form.Field name="packageAmount">
        {(field) => (
          <MoneyField
            id={"package-amount"}
            label={adminText("Package price")}
            value={field.state.value}
            onChange={field.handleChange}
            placeholder="10.00"
          />
        )}
      </form.Field>
      <form.Field name="packageSize">
        {(field) => (
          <TextField
            id={"package-size"}
            label={adminText("Units per package")}
            value={field.state.value}
            onChange={field.handleChange}
            inputMode="numeric"
            placeholder="1000"
          />
        )}
      </form.Field>
      <form.Field name="freeUnits">
        {(field) => (
          <TextField
            id={"package-free"}
            label={adminText("Free units")}
            value={field.state.value}
            onChange={field.handleChange}
            inputMode="numeric"
            placeholder="0"
          />
        )}
      </form.Field>
    </div>
  )
}

function FilterFields({ form, meter }: { form: MeteringForm; meter: Meter }) {
  const { id: adminId, text: adminText } = useAdminUI()

  const formIssue = React.useContext(RateCardIssueContext)
  return (
    <section className="grid gap-3 border-t pt-5">
      <div>
        <h3 className="text-sm font-medium">
          <AdminMessage id={"Event filters"} />
        </h3>
        <p className="text-xs text-muted-foreground">
          <AdminMessage
            id={
              "Optional. Only events with one of the listed dimension values are rated."
            }
          />
        </p>
      </div>
      <form.Field name="filters" mode="array">
        {(field) => (
          <div className="grid gap-2">
            {field.state.value.length > 0 && (
              <div className="hidden gap-2 px-0.5 text-xs text-muted-foreground sm:grid sm:grid-cols-[1fr_2fr_auto]">
                <span>
                  <AdminMessage id={"Dimension"} />
                </span>
                <span>
                  <AdminMessage id={"Values"} />
                </span>
                <span className="size-8" aria-hidden="true" />
              </div>
            )}
            {field.state.value.map((row, index) => (
              <div
                key={index}
                className="grid gap-2 sm:grid-cols-[1fr_2fr_auto]"
              >
                <div className="grid gap-1.5">
                  <Label
                    htmlFor={adminId(`filter-${index}-dimension`)}
                    className="text-xs text-muted-foreground sm:sr-only"
                  >
                    <AdminMessage id={"Dimension"} />
                  </Label>
                  <Select
                    value={row.key || null}
                    onValueChange={(value) =>
                      form.setFieldValue(`filters[${index}].key`, value ?? "")
                    }
                  >
                    <SelectTrigger
                      id={adminId(`filter-${index}-dimension`)}
                      className="w-full"
                      {...fieldIssueProps(
                        `filter-${index}-dimension`,
                        formIssue
                      )}
                    >
                      <SelectValue placeholder={adminText("Dimension")} />
                    </SelectTrigger>
                    <SelectContent>
                      {Object.keys(meter.group_by ?? {}).map((key) => (
                        <SelectItem key={key} value={key}>
                          {key}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <FieldIssue fieldId={`filter-${index}-dimension`} />
                </div>
                <RowTextField
                  id={`filter-${index}-values`}
                  label={adminText("Values")}
                  value={row.value}
                  onChange={(value) =>
                    form.setFieldValue(`filters[${index}].value`, value)
                  }
                  placeholder={adminText("eu, us")}
                />
                <RemoveRowButton
                  label={`Remove filter ${index + 1}`}
                  onClick={() => field.removeValue(index)}
                />
              </div>
            ))}
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="w-fit"
              disabled={Object.keys(meter.group_by ?? {}).length === 0}
              onClick={() => field.pushValue({ key: "", value: "" })}
            >
              <HugeiconsIcon icon={Add01Icon} className="size-4" />
              <AdminMessage id={"Add filter"} />
            </Button>
          </div>
        )}
      </form.Field>
    </section>
  )
}

function AllowanceFields({ form }: { form: MeteringForm }) {
  const { id: adminId, text: adminText } = useAdminUI()
  return (
    <section className="grid gap-4 border-t pt-5">
      <h3 className="text-sm font-medium">
        <AdminMessage id={"Allowance"} />
      </h3>
      <form.Field name="allowanceMode">
        {(field) => (
          <div className="grid gap-1.5">
            <Label htmlFor={adminId("allowance-mode")}>
              <AdminMessage id={"Included usage"} />
            </Label>
            <Select
              value={field.state.value}
              onValueChange={(value) =>
                field.handleChange(value as "none" | "included")
              }
            >
              <SelectTrigger id={adminId("allowance-mode")} className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="none">
                  <AdminMessage id="No allowance" />
                </SelectItem>
                <SelectItem value="included">
                  <AdminMessage id="Fixed included units" />
                </SelectItem>
              </SelectContent>
            </Select>
          </div>
        )}
      </form.Field>
      <form.Subscribe selector={(state) => state.values.allowanceMode}>
        {(mode) =>
          mode === "included" ? (
            <form.Field name="allowanceIncluded">
              {(field) => (
                <TextField
                  id={"allowance-included"}
                  label={adminText("Included units per period")}
                  value={field.state.value}
                  onChange={field.handleChange}
                  inputMode="numeric"
                  placeholder="1000"
                />
              )}
            </form.Field>
          ) : null
        }
      </form.Subscribe>
    </section>
  )
}

function RateSummary({ values }: { values: RateCardFormValuesCompat }) {
  let summary = "Complete the required fields to preview this rate."
  try {
    const request = buildRateCardRequest(values)
    summary = summarizeRateCard({
      price: request.price,
    } as Meter["rate_card"])
    if (request.allowance?.included !== undefined) {
      summary += ` · ${request.allowance.included.toLocaleString()} included`
    }
  } catch {
    // Incomplete form state is expected while editing.
  }
  return (
    <section className="rounded-lg bg-muted/50 px-3 py-2.5" aria-live="polite">
      <p className="text-xs font-medium text-muted-foreground">
        <AdminMessage id={"Rate preview"} />
      </p>
      <p className="mt-1 text-sm font-medium tabular-nums">{summary}</p>
    </section>
  )
}

function TextField({
  id,
  label,
  value,
  onChange,
  placeholder,
  inputMode,
}: {
  id: string
  label: string
  value: string
  onChange: (value: string) => void
  placeholder?: string
  inputMode?: "numeric" | "decimal"
}) {
  const { id: adminId } = useAdminUI()

  const issue = useRateCardIssue(id)
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={adminId(id)}>{label}</Label>
      <Input
        id={adminId(id)}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
        inputMode={inputMode}
        autoComplete="off"
        {...fieldIssueProps(id, issue ? { fieldId: id, message: issue } : null)}
      />
      <FieldIssue fieldId={id} />
    </div>
  )
}

function MoneyField(props: React.ComponentProps<typeof TextField>) {
  return <TextField {...props} inputMode="decimal" />
}

function RowTextField({
  id,
  label,
  value,
  onChange,
  placeholder,
  inputMode,
}: {
  id: string
  label: string
  value: string
  onChange: (value: string) => void
  placeholder?: string
  inputMode?: "numeric" | "decimal"
}) {
  const { id: adminId } = useAdminUI()

  const issue = useRateCardIssue(id)
  return (
    <div className="grid gap-1.5">
      <Label
        htmlFor={adminId(id)}
        className="text-xs text-muted-foreground sm:sr-only"
      >
        {label}
      </Label>
      <Input
        id={adminId(id)}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
        inputMode={inputMode}
        autoComplete="off"
        {...fieldIssueProps(id, issue ? { fieldId: id, message: issue } : null)}
      />
      <FieldIssue fieldId={id} />
    </div>
  )
}

function useRateCardIssue(fieldId: string): string | undefined {
  const issue = React.useContext(RateCardIssueContext)
  return issue?.fieldId === fieldId ? issue.message : undefined
}

function fieldIssueProps(fieldId: string, issue: FormIssue | null) {
  const invalid = issue?.fieldId === fieldId
  return {
    "aria-invalid": invalid || undefined,
    "aria-describedby": invalid ? `${fieldId}-error` : undefined,
  }
}

function FieldIssue({ fieldId }: { fieldId: string }) {
  const { id: adminId } = useAdminUI()

  const message = useRateCardIssue(fieldId)
  if (!message) return null
  return (
    <p id={adminId(`${fieldId}-error`)} className="text-xs text-destructive">
      {message}
    </p>
  )
}

function RemoveRowButton({
  label,
  onClick,
  disabled,
}: {
  label: string
  onClick: () => void
  disabled?: boolean
}) {
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      aria-label={label}
      onClick={onClick}
      disabled={disabled}
      className="self-end"
    >
      <HugeiconsIcon icon={Delete02Icon} className="size-4" />
    </Button>
  )
}
