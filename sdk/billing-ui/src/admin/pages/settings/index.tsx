import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminContext } from "#orck/admin/context"
import { useAdminServices } from "#orck/admin/context"
import * as React from "react"
import { useSearchParams } from "#orck/admin/router"

import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { Badge } from "#orck/admin/components/ui/badge"
import { TypedConfirmDialog } from "#orck/admin/components/typed-confirm-dialog"
import { FormFieldErrors } from "#orck/admin/components/form-field-errors"
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "#orck/admin/components/ui/tabs"
import type {
  MerchantConfigurationState,
  PSP,
  PSPCredential,
  RailDefinition,
} from "#orck/client/generated/wire"
import {
  amountFromInput,
  currencyScale,
  formatDate,
  formatNativeAmount,
} from "#orck/admin/lib/format"
import { DIALOG_FORM } from "#orck/admin/lib/dialog-width"

import { ApiError } from "#orck/admin/lib/api/client"

import {
  settingsTab,
  useAdminArea,
  useAdminUpdates,
  useMerchantConfig,
  useMerchantConfigEdits,
} from "#orck/admin/lib/capabilities"
import { useConfigurationEdit } from "./configuration-edit"
import {
  EditActions,
  ReadOnlyNotice,
  SettingDetail,
  SettingEditField,
} from "./setting-fields"
import { NotificationsTab } from "./notifications"

const LINE_TAB =
  "flex-none px-0 after:bg-primary group-data-horizontal/tabs:after:bottom-[-1px]"

export function SettingsPage() {
  // The tab lives in the URL so a settings page can be linked to, and so the
  // back button steps through tabs the way it looks like it should.
  const [params, setParams] = useSearchParams()
  const config = useMerchantConfig()
  const admin = useAdminArea()
  const hosted = useAdminContext().props.settingsTabs ?? []
  const values = hosted.map((h) => h.value)
  const first = settingsTab(null, config, admin, values)
  const tab = settingsTab(params.get("tab"), config, admin, values)

  return (
    <Tabs
      value={tab}
      onValueChange={(next) => {
        const updated = new URLSearchParams(params)
        if (!next || next === first) updated.delete("tab")
        else updated.set("tab", next)
        setParams(updated)
      }}
      className="flex flex-col gap-4"
    >
      <div className="overflow-x-auto">
        <TabsList
          variant="line"
          className="w-max min-w-full justify-start gap-6 rounded-none p-0"
        >
          {config && (
            <TabsTrigger value="merchant" className={LINE_TAB}>
              <AdminMessage id={"Merchant"} />
            </TabsTrigger>
          )}
          {config && (
            <TabsTrigger value="notifications" className={LINE_TAB}>
              <AdminMessage id={"Notifications"} />
            </TabsTrigger>
          )}
          {config && (
            <TabsTrigger value="psps" className={LINE_TAB}>
              <AdminMessage id={"PSPs"} />
            </TabsTrigger>
          )}
          {admin && (
            <TabsTrigger value="customer-controls" className={LINE_TAB}>
              <AdminMessage id={"Customer controls"} />
            </TabsTrigger>
          )}
          {hosted.map((h) => (
            <TabsTrigger key={h.value} value={h.value} className={LINE_TAB}>
              {h.title}
            </TabsTrigger>
          ))}
        </TabsList>
      </div>
      {config && (
        <TabsContent value="merchant">
          <MerchantSettingsTab />
        </TabsContent>
      )}
      {config && (
        <TabsContent value="notifications">
          <NotificationsTab />
        </TabsContent>
      )}
      {config && (
        <TabsContent value="psps">
          <PSPsTab />
        </TabsContent>
      )}
      {admin && (
        <TabsContent value="customer-controls">
          <CustomerControlsTab />
        </TabsContent>
      )}
      {hosted.map((h) => (
        <TabsContent key={h.value} value={h.value}>
          {h.element}
        </TabsContent>
      ))}
    </Tabs>
  )
}

export function MerchantSettingsTab() {
  const { adminQueries } = useAdminServices()

  const { data } = useQuery(adminQueries.merchantConfiguration("Load settings"))
  const edits = useMerchantConfigEdits()
  if (!data)
    return (
      <p className="text-sm text-muted-foreground">
        <AdminMessage id={"Loading…"} />
      </p>
    )
  return (
    <div className="grid gap-10">
      <ReadOnlyNotice />
      <MerchantProfileForm configuration={data} edits={edits} />
      <RepriceNoticeWindowForm configuration={data} edits={edits} />
    </div>
  )
}

function MerchantProfileForm({
  configuration,
  edits,
}: {
  configuration: MerchantConfigurationState
  edits: boolean
}) {
  const { id: adminId, text: adminText } = useAdminUI()

  // The merchant's one name is the configuration's display_name.
  const profile = configuration.profile
  const current = () => ({
    displayName: configuration.display_name,
    fromEmail: profile?.from_email ?? "",
    supportURL: profile?.support_url ?? "",
    subscribeURL: profile?.subscribe_url ?? "",
    billingURL: profile?.billing_url ?? "",
    logoURL: profile?.logo_url ?? "",
  })
  const edit = useConfigurationEdit(configuration)
  const form = useForm({
    defaultValues: current(),
    onSubmit: ({ value }) =>
      edit.save(
        {
          display_name: value.displayName.trim(),
          profile: {
            from_email: value.fromEmail || undefined,
            support_url: value.supportURL || undefined,
            subscribe_url: value.subscribeURL || undefined,
            billing_url: value.billingURL || undefined,
            logo_url: value.logoURL || undefined,
          },
        },
        "Profile saved",
        "Save profile"
      ),
  })

  return (
    <section className="grid gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid gap-1">
          <h2 className="text-base font-semibold">
            <AdminMessage id={"Merchant profile"} />
          </h2>
          <p className="text-sm text-pretty text-muted-foreground">
            <AdminMessage
              id={
                "Customer-facing merchant details used on invoices and emails."
              }
            />
          </p>
        </div>
        <form.Subscribe
          selector={(state) => [state.canSubmit, state.isSubmitting]}
        >
          {([canSubmit, isSubmitting]) => (
            <EditActions
              edits={edits}
              editing={edit.editing}
              form="merchant-profile-form"
              saving={isSubmitting}
              canSave={canSubmit}
              onEdit={() => {
                form.reset(current())
                edit.open()
              }}
              onCancel={edit.close}
            />
          )}
        </form.Subscribe>
      </div>

      {edit.editing ? (
        <form
          id={adminId("merchant-profile-form")}
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            void form.handleSubmit()
          }}
          className="grid gap-4"
        >
          <form.Field
            name="displayName"
            validators={{
              onChange: ({ value }) =>
                value.trim() ? undefined : "Enter the merchant's name",
            }}
          >
            {(field) => (
              <SettingEditField label={adminText("Display name")} id={"s-name"}>
                <div className="grid gap-1.5">
                  <Input
                    id={adminId("s-name")}
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    autoComplete="organization"
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
                  <FormFieldErrors errors={field.state.meta.errors} />
                </div>
              </SettingEditField>
            )}
          </form.Field>
          <form.Field name="fromEmail">
            {(field) => (
              <SettingEditField label={adminText("From email")} id={"s-email"}>
                <Input
                  id={adminId("s-email")}
                  type="email"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  autoComplete="email"
                />
              </SettingEditField>
            )}
          </form.Field>
          <form.Field name="supportURL">
            {(field) => (
              <SettingEditField
                label={adminText("Support URL")}
                id={"s-support"}
              >
                <Input
                  id={adminId("s-support")}
                  type="url"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  placeholder={adminText("https://example.com/support")}
                />
              </SettingEditField>
            )}
          </form.Field>
          <form.Field name="subscribeURL">
            {(field) => (
              <SettingEditField
                label={adminText("Subscribe URL")}
                id={"s-subscribe"}
              >
                <Input
                  id={adminId("s-subscribe")}
                  type="url"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  placeholder={adminText("https://example.com/subscribe")}
                />
              </SettingEditField>
            )}
          </form.Field>
          <form.Field name="billingURL">
            {(field) => (
              <SettingEditField
                label={adminText("Billing URL")}
                id={"s-billing"}
              >
                <Input
                  id={adminId("s-billing")}
                  type="url"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  placeholder={adminText("https://example.com/account")}
                />
              </SettingEditField>
            )}
          </form.Field>
          <form.Field name="logoURL">
            {(field) => (
              <SettingEditField label={adminText("Logo URL")} id={"s-logo"}>
                <Input
                  id={adminId("s-logo")}
                  type="url"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  placeholder={adminText("https://example.com/logo.png")}
                />
              </SettingEditField>
            )}
          </form.Field>
        </form>
      ) : (
        <dl className="grid gap-4">
          <SettingDetail
            label={adminText("Display name")}
            value={configuration.display_name}
          />
          <SettingDetail
            label={adminText("From email")}
            value={profile?.from_email}
          />
          <SettingDetail
            label={adminText("Support URL")}
            value={profile?.support_url}
          />
          <SettingDetail
            label={adminText("Subscribe URL")}
            value={profile?.subscribe_url}
          />
          <SettingDetail
            label={adminText("Billing URL")}
            value={profile?.billing_url}
          />
          <SettingDetail
            label={adminText("Logo URL")}
            value={profile?.logo_url}
          />
        </dl>
      )}
    </section>
  )
}

// RepriceNoticeWindowForm (#781): the minimum advance notice (days) a
// subscription price increase must give existing subscribers. The price-change
// wizard reads the same value; the API enforces it regardless.
function RepriceNoticeWindowForm({
  configuration,
  edits,
}: {
  configuration: MerchantConfigurationState
  edits: boolean
}) {
  const { id: adminId, text: adminText } = useAdminUI()

  const days = configuration.reprice_notice_window_days ?? 30
  const edit = useConfigurationEdit(configuration)
  const form = useForm({
    defaultValues: { days: String(days) },
    onSubmit: ({ value }) =>
      edit.save(
        { reprice_notice_window_days: Number(value.days) },
        "Notice window saved",
        "Save notice window"
      ),
  })

  return (
    <section className="grid gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid max-w-2xl gap-1">
          <h2 className="text-base font-semibold">
            <AdminMessage id={"Pricing policies"} />
          </h2>
          <p className="text-sm text-pretty text-muted-foreground">
            <AdminMessage
              id={
                "Set how much notice customers receive before a price increase."
              }
            />
          </p>
        </div>
        <form.Subscribe
          selector={(state) => [state.canSubmit, state.isSubmitting]}
        >
          {([canSubmit, isSubmitting]) => (
            <EditActions
              edits={edits}
              editing={edit.editing}
              form="notice-window-form"
              saving={isSubmitting}
              canSave={canSubmit}
              onEdit={() => {
                form.reset({ days: String(days) })
                edit.open()
              }}
              onCancel={edit.close}
            />
          )}
        </form.Subscribe>
      </div>

      {edit.editing ? (
        <form
          id={adminId("notice-window-form")}
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            void form.handleSubmit()
          }}
        >
          <form.Field
            name="days"
            validators={{
              onChange: ({ value }) => {
                const days = Number(value)
                return value.trim() && Number.isInteger(days) && days >= 0
                  ? undefined
                  : "Enter a whole number of days"
              },
            }}
          >
            {(field) => (
              <SettingEditField
                label={adminText("Notice period (days)")}
                id={"s-notice-window"}
              >
                <div className="grid gap-1.5">
                  <Input
                    id={adminId("s-notice-window")}
                    type="number"
                    step="1"
                    min="0"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
                  <FormFieldErrors errors={field.state.meta.errors} />
                </div>
              </SettingEditField>
            )}
          </form.Field>
        </form>
      ) : (
        <dl>
          <SettingDetail
            label={adminText("Notice period")}
            value={`${days} ${days === 1 ? "day" : "days"}`}
          />
        </dl>
      )}
    </section>
  )
}

export function PSPsTab() {
  const { selectedMerchant, adminQueries } = useAdminServices()

  const psps = useQuery(adminQueries.psps())
  const rails = useQuery(adminQueries.rails())
  const edits = useMerchantConfigEdits()
  if (psps.isPending || rails.isPending)
    return (
      <p className="text-sm text-muted-foreground">
        <AdminMessage id={"Loading…"} />
      </p>
    )

  const railDefinitions = rails.data ?? []

  return (
    <div className="grid gap-6">
      <ReadOnlyNotice />
      <section className="grid gap-5">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="grid gap-1">
            <h2 className="text-base font-semibold">
              <AdminMessage id={"PSPs"} />
            </h2>
            <p className="text-sm text-muted-foreground">
              <AdminMessage
                id={
                  "The accounts this merchant takes payments through, one or more per rail."
                }
              />
            </p>
          </div>
          {edits && (
            <PSPDialog
              key={selectedMerchant() ?? ""}
              railDefinitions={railDefinitions}
            />
          )}
        </div>
        {!psps.data?.data?.length ? (
          <p className="py-2 text-sm text-muted-foreground">
            <AdminMessage id={"No PSPs configured."} />
          </p>
        ) : (
          <Table className="min-w-[44rem]">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="text-muted-foreground">PSP</TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Environment"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Credentials"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"State"} />
                </TableHead>
                {edits && (
                  <TableHead className="text-right text-muted-foreground">
                    <AdminMessage id={"Action"} />
                  </TableHead>
                )}
              </TableRow>
            </TableHeader>
            <TableBody>
              {psps.data.data.map((psp) => (
                <PSPRow
                  key={psp.id}
                  psp={psp}
                  railDefinitions={railDefinitions}
                  edits={edits}
                />
              ))}
            </TableBody>
          </Table>
        )}
      </section>
    </div>
  )
}

// Credentials are write-only: a PSP shows whether each is set, never a value.
function credentialTitle(c: PSPCredential) {
  if (c.validated_at) return `Validated ${formatDate(c.validated_at)}`
  return c.configured ? "Configured" : "Not configured"
}

function PSPRow({
  psp,
  railDefinitions,
  edits,
}: {
  psp: PSP
  railDefinitions: RailDefinition[]
  edits: boolean
}) {
  const {
    toast,
    toastApiError,
    toastStaleEdit,
    adminMutations,
    selectedMerchant,
  } = useAdminServices()

  const { text: adminText } = useAdminUI()

  const queryClient = useQueryClient()
  const updatePSP = useMutation(adminMutations.updatePSP(queryClient))
  const [confirmLastOpen, setConfirmLastOpen] = React.useState(false)
  const confirm = useConfirm()
  const definition = railDefinitions.find((d) => d.rail === psp.rail)
  const railName = definition?.display_name ?? psp.rail
  const credentials = psp.credentials ?? {}
  // Archive exactly this PSP (#655). It never contacts the provider, so a
  // terminated account archives too. The rail's last active PSP needs an
  // explicit confirmation.
  const archive = async (allowLast: boolean) => {
    if (
      !allowLast &&
      !(await confirm({
        title: `Archive ${psp.key}?`,
        description:
          "New purchase stops routing to it. Existing subscriptions, refunds and webhooks keep using it.",
        confirmLabel: "Archive PSP",
      }))
    )
      return
    try {
      await updatePSP.mutateAsync({
        id: psp.id,
        psp: {
          archived: true,
          expected_revision: psp.revision,
          ...(allowLast ? { allow_last: true } : {}),
        },
      })
      toast.success("PSP archived")
    } catch (err) {
      if (err instanceof ApiError && err.code === "psp_last_active") {
        if (!allowLast) setConfirmLastOpen(true)
        else toastApiError(err, "Archive PSP")
        return
      }
      if (err instanceof ApiError && err.revisionMismatch) {
        toastStaleEdit(`PSP ${psp.key}`)
        return
      }
      toastApiError(err, "Archive PSP")
    }
  }
  return (
    <TableRow className={psp.archived ? "opacity-60" : undefined}>
      <TableCell className="py-3">
        <div className="grid min-w-0 leading-tight">
          <span className="font-medium">{psp.key}</span>
          <span className="truncate text-xs text-muted-foreground">
            {railName} · {pspAccount(psp)}
          </span>
        </div>
      </TableCell>
      <TableCell className="py-3 capitalize">{psp.environment}</TableCell>
      <TableCell className="py-3">
        {Object.keys(credentials).length > 0 ? (
          <span className="flex flex-wrap gap-1">
            {Object.entries(credentials).map(([name, credential]) => (
              <Badge
                key={name}
                variant="secondary"
                className={
                  credential.configured ? "" : "bg-held-surface text-held"
                }
                title={credentialTitle(credential)}
              >
                {name}
              </Badge>
            ))}
          </span>
        ) : (
          <span className="text-sm text-muted-foreground">
            <AdminMessage id={"None required"} />
          </span>
        )}
      </TableCell>
      <TableCell className="py-3">
        {psp.archived && psp.open_obligations === 0 ? (
          <Badge variant="secondary">
            <AdminMessage id={"archived"} />
          </Badge>
        ) : psp.archived ? (
          <Badge variant="secondary" className="bg-held-surface text-held">
            <AdminMessage id={"draining ("} />
            {psp.open_obligations})
          </Badge>
        ) : (
          <Badge
            variant="secondary"
            className="bg-settled-surface text-settled"
          >
            <AdminMessage id={"active"} />
          </Badge>
        )}
      </TableCell>
      {edits && (
        <TableCell className="py-3 text-right">
          {!psp.archived && (
            <div className="flex justify-end gap-2">
              <RotateCredentialsDialog
                key={`${selectedMerchant() ?? ""}:${psp.id}`}
                psp={psp}
                credentialKeys={
                  definition?.credential_keys ?? Object.keys(credentials)
                }
              />
              <Button
                variant="outline"
                size="sm"
                disabled={updatePSP.isPending}
                onClick={() => archive(false)}
              >
                <AdminMessage id={"Archive"} />
              </Button>
              <TypedConfirmDialog
                open={confirmLastOpen}
                onOpenChange={setConfirmLastOpen}
                title={`Archive the last active ${railName} PSP?`}
                description={adminText(
                  "New purchase on this rail is refused until another PSP is armed. Existing subscriptions, refunds and webhooks keep using this one."
                )}
                confirmationWord="ARCHIVE"
                actionLabel="Archive PSP"
                onConfirm={() => archive(true)}
              />
            </div>
          )}
        </TableCell>
      )}
    </TableRow>
  )
}

// RotateCredentialsDialog writes new credentials; the current ones are never
// shown. The provider checks the new ones before anything is stored, so a
// rejected credential changes nothing. The edit names the revision read when
// the dialog opened, and secret fields clear whenever it closes.
export function RotateCredentialsDialog({
  psp,
  credentialKeys,
}: {
  psp: PSP
  credentialKeys: string[]
}) {
  const {
    toast,
    toastApiError,
    toastStaleEdit,
    adminMutations,
    selectedMerchant,
  } = useAdminServices()

  const { id: adminId } = useAdminUI()

  const [open, setOpen] = React.useState(false)
  const [merchant] = React.useState(() => selectedMerchant() ?? "")
  const revision = React.useRef(psp.revision)
  const queryClient = useQueryClient()
  const updatePSP = useMutation(adminMutations.updatePSP(queryClient))
  const credentials = psp.credentials ?? {}
  const form = useForm({
    defaultValues: { credentials: {} as Record<string, string> },
    onSubmit: async ({ value }) => {
      const supplied = Object.fromEntries(
        Object.entries(value.credentials).filter(([, item]) => item.trim())
      )
      try {
        if ((selectedMerchant() ?? "") !== merchant)
          throw new Error("Merchant changed; reopen this form")
        await updatePSP.mutateAsync({
          id: psp.id,
          psp: { expected_revision: revision.current, credentials: supplied },
        })
        toast.success(`${psp.key} credentials validated and rotated`)
        close(false)
      } catch (err) {
        if (err instanceof ApiError && err.revisionMismatch) {
          toastStaleEdit(`PSP ${psp.key}`)
          close(false)
          return
        }
        toastApiError(err, "Rotate credentials")
      } finally {
        updatePSP.reset()
      }
    },
  })

  const close = (next: boolean) => {
    if (!next) form.reset()
    if (next) revision.current = psp.revision
    setOpen(next)
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            <AdminMessage id={"Rotate"} />
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id={"Rotate "} />
            {psp.key}
            <AdminMessage id={" credentials · "} />
            {pspAccount(psp)}
          </DialogTitle>
          <DialogDescription>
            <AdminMessage
              id={
                "Current credentials are never shown. A new one is validated against the live provider before it is stored; if that check fails, nothing is written. Leave a field blank to keep the credential it holds now."
              }
            />
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            void form.handleSubmit()
          }}
          className="grid gap-4"
        >
          <form.Field name="credentials">
            {(field) => (
              <div className="grid gap-3">
                {credentialKeys.map((name) => {
                  const current = credentials[name]
                  return (
                    <Field key={name} label={name} id={`rot-${psp.id}-${name}`}>
                      <Input
                        id={adminId(`rot-${psp.id}-${name}`)}
                        type="password"
                        autoComplete="new-password"
                        placeholder={
                          current?.configured ? "unchanged" : "not configured"
                        }
                        value={field.state.value[name] ?? ""}
                        onChange={(event) =>
                          field.handleChange({
                            ...field.state.value,
                            [name]: event.target.value,
                          })
                        }
                      />
                      {current?.validated_at && (
                        <p className="text-xs text-muted-foreground">
                          <AdminMessage id={"Last validated "} />
                          {formatDate(current.validated_at)}
                        </p>
                      )}
                    </Field>
                  )
                })}
                <p className="text-xs text-muted-foreground">
                  <AdminMessage
                    id={
                      "Once this succeeds, the new credential is used for the next charge and every one after it. Previous webhook signing secrets remain accepted while their required overlap is active."
                    }
                  />
                </p>
              </div>
            )}
          </form.Field>
          <DialogFooter>
            <form.Subscribe
              selector={(state) =>
                [state.values.credentials, state.isSubmitting] as const
              }
            >
              {([values, isSubmitting]) => (
                <Button
                  type="submit"
                  disabled={
                    isSubmitting ||
                    !Object.values(values).some((value) => value.trim())
                  }
                >
                  {isSubmitting ? "Validating…" : "Validate & rotate"}
                </Button>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// Each rail names its account in its own terms, read off its own dashboard.
// Naming where is the only way the field is answerable without a support
// ticket.
const ACCOUNT_FIELDS: Record<string, { label: string; hint: string }> = {
  gateway_id: {
    label: "Gateway ID",
    hint: "The Gateway ID shown in your NMI dashboard.",
  },
  account_id: {
    label: "Account ID",
    hint: "Your Stripe account ID. It starts with acct_.",
  },
  client_accnum: {
    label: "Client account number",
    hint: "Your CCBill account number, such as 945280.",
  },
  client_subacc: {
    label: "Client subaccount",
    hint: "Your CCBill subaccount number, such as 0000.",
  },
  treasury: {
    label: "Treasury",
    hint: "The Solana address your payments land in.",
  },
}

// The PSP's account as its rail names it; CCBill's two numbers read as one.
function pspAccount(psp: PSP): string {
  if (psp.client_accnum)
    return `${psp.client_accnum}-${psp.client_subacc ?? ""}`
  return psp.gateway_id ?? psp.account_id ?? psp.treasury ?? ""
}

// What each environment means for money, in the merchant's words.
const ENVIRONMENT_LABELS: Record<string, string> = {
  live: "Live: real money",
  test: "Test: the provider's own system, no money moves",
  sandbox: "Sandbox: the provider's separate sandbox",
  mainnet: "Mainnet: real money",
  devnet: "Devnet: Solana's development network",
  testnet: "Testnet: Solana's test network",
}

const RAIL_NAMES: Record<string, string> = {
  nmi: "NMI",
  ccbill: "CCBill",
  stripe: "Stripe",
  solana: "Solana",
}

// Several rails carry the same display name ("Credit Card"), so the rail has to
// stay visible or the list offers the same option twice.
function railProviderLabel(rail: string, displayName: string): string {
  const railName = RAIL_NAMES[rail] ?? rail
  if (!displayName || displayName.toLowerCase() === railName.toLowerCase()) {
    return railName
  }
  return `${displayName} (${railName})`
}

// Credential names arrive as storage keys. Read them as words, and keep the
// acronyms the merchant sees on the provider's own dashboard.
const CREDENTIAL_ACRONYMS: Record<string, string> = {
  api: "API",
  id: "ID",
  url: "URL",
}

function credentialLabel(name: string): string {
  const words = name.split(/[_-]+/).filter(Boolean)
  return words
    .map((word, index) => {
      const acronym = CREDENTIAL_ACRONYMS[word.toLowerCase()]
      if (acronym) return acronym
      if (index > 0) return word.toLowerCase()
      return word.charAt(0).toUpperCase() + word.slice(1).toLowerCase()
    })
    .join(" ")
}

function PSPDialog({ railDefinitions }: { railDefinitions: RailDefinition[] }) {
  const { toast, toastApiError, adminMutations, selectedMerchant } =
    useAdminServices()

  const { text: adminText, id: adminId } = useAdminUI()

  const [open, setOpen] = React.useState(false)
  const [merchant] = React.useState(() => selectedMerchant() ?? "")
  const queryClient = useQueryClient()
  const createPSP = useMutation(adminMutations.createPSP(queryClient))
  const form = useForm({
    defaultValues: {
      rail: "",
      environment: "",
      key: "",
      account: {} as Record<string, string>,
      credentials: {} as Record<string, string>,
    },
    onSubmit: async ({ value }) => {
      const credentials = Object.fromEntries(
        Object.entries(value.credentials).filter(([, item]) => item !== "")
      )
      try {
        if ((selectedMerchant() ?? "") !== merchant)
          throw new Error("Merchant changed; reopen this form")
        const definition = railDefinitions.find((d) => d.rail === value.rail)
        await createPSP.mutateAsync({
          key: value.key.trim().toLowerCase(),
          rail: value.rail as PSP["rail"],
          environment: value.environment,
          ...Object.fromEntries(
            (definition?.account_fields ?? []).map((name) => [
              name,
              (value.account[name] ?? "").trim(),
            ])
          ),
          ...(Object.keys(credentials).length ? { credentials } : {}),
        })
        form.reset()
        toast.success("PSP added")
        setOpen(false)
      } catch (err) {
        toastApiError(err, "Add PSP")
      } finally {
        createPSP.reset()
      }
    },
  })

  const handleOpenChange = (next: boolean) => {
    if (!next) form.reset()
    setOpen(next)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          <Button size="sm">
            <AdminMessage id={"Add PSP"} />
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id={"Add a PSP"} />
          </DialogTitle>
          <DialogDescription>
            <AdminMessage
              id={
                "Connect the account that will take money for you. Credentials go straight into the secret store and are never shown again."
              }
            />
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            void form.handleSubmit()
          }}
          className="grid gap-4"
        >
          <div className="grid gap-3">
            <form.Field
              name="rail"
              validators={{
                onChange: ({ value }) =>
                  value ? undefined : "Choose a payment rail",
              }}
            >
              {(field) => (
                <Field label={adminText("Payment rail")} id={"pv-rail"}>
                  <Select
                    items={railDefinitions.map((definition) => ({
                      value: definition.rail,
                      label: railProviderLabel(
                        definition.rail,
                        definition.display_name
                      ),
                    }))}
                    value={field.state.value || null}
                    onValueChange={(value) => {
                      field.handleChange(value ?? "")
                      // Each rail names its account, asks for credentials and
                      // has environments of its own, so none carries over; its
                      // real-money environment is the default.
                      form.setFieldValue("account", {})
                      form.setFieldValue("credentials", {})
                      form.setFieldValue(
                        "environment",
                        railDefinitions.find((d) => d.rail === value)
                          ?.environments[0] ?? ""
                      )
                    }}
                  >
                    <SelectTrigger
                      id={adminId("pv-rail")}
                      className="w-full"
                      aria-invalid={field.state.meta.errors.length > 0}
                    >
                      <SelectValue placeholder={adminText("Pick a rail…")} />
                    </SelectTrigger>
                    <SelectContent>
                      {railDefinitions.map((definition) => (
                        <SelectItem
                          key={definition.rail}
                          value={definition.rail}
                        >
                          {railProviderLabel(
                            definition.rail,
                            definition.display_name
                          )}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <FormFieldErrors errors={field.state.meta.errors} />
                </Field>
              )}
            </form.Field>
            <form.Subscribe selector={(state) => state.values.rail}>
              {(rail) => {
                const environments =
                  railDefinitions.find((definition) => definition.rail === rail)
                    ?.environments ?? []
                return (
                  <form.Field
                    name="environment"
                    validators={{
                      onChange: ({ value }) =>
                        value
                          ? undefined
                          : "Choose whether it moves real money",
                    }}
                  >
                    {(field) => (
                      <Field
                        label={adminText("Environment")}
                        id={"pv-environment"}
                        hint="Whether this PSP moves real money. It is checked with the provider before the PSP is used."
                      >
                        <Select
                          items={environments.map((environment) => ({
                            value: environment,
                            label: adminText(
                              ENVIRONMENT_LABELS[environment] ?? environment
                            ),
                          }))}
                          value={field.state.value || null}
                          onValueChange={(value) =>
                            field.handleChange(value ?? "")
                          }
                        >
                          <SelectTrigger
                            id={adminId("pv-environment")}
                            className="w-full"
                            disabled={!rail}
                            aria-invalid={field.state.meta.errors.length > 0}
                          >
                            <SelectValue
                              placeholder={adminText("Pick an environment…")}
                            />
                          </SelectTrigger>
                          <SelectContent>
                            {environments.map((environment) => (
                              <SelectItem key={environment} value={environment}>
                                {adminText(
                                  ENVIRONMENT_LABELS[environment] ?? environment
                                )}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                        <FormFieldErrors errors={field.state.meta.errors} />
                      </Field>
                    )}
                  </form.Field>
                )
              }}
            </form.Subscribe>
            <form.Field
              name="key"
              validators={{
                onChange: ({ value }) =>
                  /^[a-z0-9][a-z0-9_-]{0,62}$/.test(value.trim().toLowerCase())
                    ? undefined
                    : "Lowercase letters, digits, - or _",
              }}
            >
              {(field) => (
                <Field
                  label={adminText("Key")}
                  id={"pv-key"}
                  hint="Your name for this PSP, such as mobius. Prices and purchase name it by this key."
                >
                  <Input
                    id={adminId("pv-key")}
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
                  <FormFieldErrors errors={field.state.meta.errors} />
                </Field>
              )}
            </form.Field>
            <form.Subscribe selector={(state) => state.values.rail}>
              {(rail) => (
                <form.Field name="account">
                  {(field) => (
                    <>
                      {railDefinitions
                        .find((d) => d.rail === rail)
                        ?.account_fields.map((name) => (
                          <Field
                            key={name}
                            label={adminText(
                              ACCOUNT_FIELDS[name]?.label ??
                                credentialLabel(name)
                            )}
                            id={`pv-account-${name}`}
                            hint={ACCOUNT_FIELDS[name]?.hint}
                          >
                            <Input
                              id={adminId(`pv-account-${name}`)}
                              value={field.state.value[name] ?? ""}
                              onBlur={field.handleBlur}
                              onChange={(event) =>
                                field.handleChange({
                                  ...field.state.value,
                                  [name]: event.target.value,
                                })
                              }
                            />
                          </Field>
                        ))}
                    </>
                  )}
                </form.Field>
              )}
            </form.Subscribe>
            <form.Subscribe selector={(state) => state.values.rail}>
              {(rail) => {
                const selectedProvider = railDefinitions.find(
                  (definition) => definition.rail === rail
                )
                return (
                  <form.Field name="credentials">
                    {(field) => (
                      <>
                        {selectedProvider?.credential_keys.map((name) => (
                          <Field
                            key={name}
                            label={credentialLabel(name)}
                            id={`pv-credential-${name}`}
                          >
                            <Input
                              id={adminId(`pv-credential-${name}`)}
                              type="password"
                              autoComplete="new-password"
                              value={field.state.value[name] ?? ""}
                              onChange={(event) =>
                                field.handleChange({
                                  ...field.state.value,
                                  [name]: event.target.value,
                                })
                              }
                            />
                          </Field>
                        ))}
                      </>
                    )}
                  </form.Field>
                )
              }}
            </form.Subscribe>
          </div>
          <DialogFooter>
            <form.Subscribe
              selector={(state) =>
                [
                  state.values.rail,
                  state.values.environment,
                  state.values.key,
                  state.values.account,
                  state.canSubmit,
                  state.isSubmitting,
                ] as const
              }
            >
              {([rail, environment, key, account, canSubmit, isSubmitting]) => (
                <>
                  <Button
                    type="button"
                    variant="outline"
                    onClick={() => handleOpenChange(false)}
                  >
                    <AdminMessage id={"Cancel"} />
                  </Button>
                  <Button
                    type="submit"
                    disabled={
                      !rail ||
                      !environment ||
                      !key.trim() ||
                      !railDefinitions
                        .find((d) => d.rail === rail)
                        ?.account_fields.every((name) =>
                          (account[name] ?? "").trim()
                        ) ||
                      !canSubmit ||
                      isSubmitting
                    }
                  >
                    {isSubmitting ? "Saving…" : "Add PSP"}
                  </Button>
                </>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function CustomerControlsTab() {
  const { toast, toastApiError, adminMutations } = useAdminServices()

  const { text: adminText, id: adminId } = useAdminUI()

  const [result, setResult] = React.useState<{
    customerID: string
    currency: string
    creditLimit: string
    trustLevel: string
  }>()
  const lookupControls = useMutation(adminMutations.lookupCustomerControls())
  const updateCreditLimit = useMutation(adminMutations.setCreditLimit())
  const canUpdate = useAdminUpdates()
  const creditForm = useForm({
    defaultValues: { newLimit: "" },
    onSubmit: async ({ value }) => {
      if (!result) return
      const scale = currencyScale(result.currency)
      const amount =
        scale === undefined ? null : amountFromInput(value.newLimit, scale)
      if (amount === null || amount.startsWith("-")) return
      try {
        await updateCreditLimit.mutateAsync({
          customerId: result.customerID,
          currency: result.currency,
          amount,
        })
        setResult((current) =>
          current ? { ...current, creditLimit: amount } : current
        )
        creditForm.reset()
        toast.success("Credit limit updated")
      } catch (err) {
        toastApiError(err, "Set credit limit")
      }
    },
  })
  const lookupForm = useForm({
    defaultValues: { customerID: "", currency: "usd" },
    onSubmit: async ({ value }) => {
      const customerID = value.customerID.trim()
      const currency = value.currency.trim().toLowerCase()
      try {
        const controls = await lookupControls.mutateAsync({
          customerId: customerID,
          currency,
        })
        setResult({ customerID, ...controls })
        creditForm.reset()
      } catch (err) {
        toastApiError(err, "Lookup customer controls")
        setResult(undefined)
      }
    },
  })

  return (
    <div className="grid gap-10">
      <section className="grid gap-5">
        <div className="grid gap-1">
          <h2 className="text-base font-semibold">
            <AdminMessage id={"Customer lookup"} />
          </h2>
          <p className="text-sm text-muted-foreground">
            <AdminMessage id={"Find a customer by ID and currency."} />
          </p>
        </div>
        <form
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            void lookupForm.handleSubmit()
          }}
          className="grid gap-4 md:grid-cols-[minmax(0,1fr)_10rem_auto] md:items-end"
        >
          <lookupForm.Field
            name="customerID"
            validators={{
              onChange: ({ value }) =>
                value.trim() ? undefined : "Enter a customer ID",
            }}
          >
            {(field) => (
              <Field
                label={adminText("Customer ID")}
                id={"customer-controls-id"}
              >
                <Input
                  id={adminId("customer-controls-id")}
                  placeholder={adminText("Customer UUID")}
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  aria-invalid={field.state.meta.errors.length > 0}
                />
                <FormFieldErrors errors={field.state.meta.errors} />
              </Field>
            )}
          </lookupForm.Field>
          <lookupForm.Field
            name="currency"
            validators={{
              onChange: ({ value }) =>
                value.trim() ? undefined : "Enter a currency",
            }}
          >
            {(field) => (
              <Field
                label={adminText("Currency")}
                id={"customer-controls-currency"}
              >
                <Input
                  id={adminId("customer-controls-currency")}
                  placeholder={adminText("USD")}
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  aria-invalid={field.state.meta.errors.length > 0}
                />
                <FormFieldErrors errors={field.state.meta.errors} />
              </Field>
            )}
          </lookupForm.Field>
          <lookupForm.Subscribe
            selector={(state) =>
              [
                state.values.customerID,
                state.values.currency,
                state.canSubmit,
                state.isSubmitting,
              ] as const
            }
          >
            {([customerID, currency, canSubmit, isSubmitting]) => (
              <Button
                type="submit"
                variant="outline"
                disabled={
                  !customerID.trim() ||
                  !currency.trim() ||
                  !canSubmit ||
                  isSubmitting
                }
              >
                {isSubmitting ? "Looking up…" : "Look up"}
              </Button>
            )}
          </lookupForm.Subscribe>
        </form>
      </section>

      {result && (
        <section className="grid gap-5">
          <div className="grid gap-1">
            <h2 className="text-base font-semibold">
              <AdminMessage id={"Credit and trust"} />
            </h2>
            <p className="text-sm text-muted-foreground">
              <span className="font-mono text-xs break-all text-foreground">
                {result.customerID}
              </span>{" "}
              · {result.currency.toUpperCase()}
            </p>
          </div>
          <dl className="grid gap-4">
            <SettingDetail
              label={adminText("Trust level")}
              value={result.trustLevel || "Default"}
            />
            <SettingDetail
              label={adminText("Credit limit")}
              value={
                result.creditLimit !== "0"
                  ? formatNativeAmount(result.creditLimit, result.currency)
                  : "Off"
              }
            />
          </dl>
          {canUpdate && (
            <form
              onSubmit={(event) => {
                event.preventDefault()
                event.stopPropagation()
                void creditForm.handleSubmit()
              }}
            >
              <SettingEditField
                label={`New limit (${result.currency.toUpperCase()})`}
                id={"customer-controls-limit"}
              >
                <creditForm.Field
                  name="newLimit"
                  validators={{
                    onChange: ({ value }) => {
                      const scale = currencyScale(result.currency)
                      const amount =
                        scale === undefined
                          ? null
                          : amountFromInput(value, scale)
                      return value !== "" &&
                        amount !== null &&
                        !amount.startsWith("-")
                        ? undefined
                        : "Enter a valid amount"
                    },
                  }}
                >
                  {(field) => (
                    <div className="grid gap-1.5">
                      <div className="flex gap-2">
                        <Input
                          id={adminId("customer-controls-limit")}
                          placeholder="0.00"
                          type="number"
                          step="any"
                          min="0"
                          value={field.state.value}
                          onBlur={field.handleBlur}
                          onChange={(event) =>
                            field.handleChange(event.target.value)
                          }
                          aria-invalid={field.state.meta.errors.length > 0}
                        />
                        <creditForm.Subscribe
                          selector={(state) =>
                            [
                              state.values.newLimit,
                              state.canSubmit,
                              state.isSubmitting,
                            ] as const
                          }
                        >
                          {([newLimit, canSubmit, isSubmitting]) => (
                            <Button
                              type="submit"
                              disabled={!newLimit || !canSubmit || isSubmitting}
                            >
                              {isSubmitting ? "Updating…" : "Update"}
                            </Button>
                          )}
                        </creditForm.Subscribe>
                      </div>
                      <FormFieldErrors errors={field.state.meta.errors} />
                      <p className="text-xs text-muted-foreground">
                        <AdminMessage id={"Enter 0 to turn credit off."} />
                      </p>
                    </div>
                  )}
                </creditForm.Field>
              </SettingEditField>
            </form>
          )}
        </section>
      )}
    </div>
  )
}

// hint carries the invisible constraint: where a value comes from, or what
// happens if it is left empty. Anything the control already says is left out.
function Field({
  label,
  id,
  hint,
  children,
}: {
  label: string
  id: string
  hint?: string
  children: React.ReactNode
}) {
  const { id: adminId } = useAdminUI()

  return (
    <div className="grid gap-1.5">
      <Label htmlFor={adminId(id)}>{label}</Label>
      {hint ? (
        <p className="text-[13px] text-muted-foreground">{hint}</p>
      ) : null}
      {children}
    </div>
  )
}
