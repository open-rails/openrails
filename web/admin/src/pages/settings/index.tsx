import * as React from "react"
import { useSearchParams } from "react-router-dom"
import { toast } from "sonner"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { Badge } from "@/components/ui/badge"
import { FormFieldErrors } from "@/components/form-field-errors"
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import type {
  PaymentProviderConfig,
  PaymentProviderDefinition,
} from "@/lib/api/types"
import { formatDate, formatMicros, microsFromInput } from "@/lib/format"
import { DIALOG_FORM } from "@/lib/dialog-width"
import { adminMutations } from "@/lib/mutations"
import { toastApiError } from "@/lib/toast"
import { adminQueries } from "@/lib/queries"
import { AlertsTab } from "./alerts"
import { ApiKeysTab } from "./api-keys"
import { TeamTab } from "./team"

const LINE_TAB =
  "flex-none px-0 after:bg-primary group-data-horizontal/tabs:after:bottom-[-1px]"

export function SettingsPage() {
  // The tab lives in the URL so a settings page can be linked to, and so the
  // back button steps through tabs the way it looks like it should.
  const [params, setParams] = useSearchParams()
  const tab = params.get("tab") || "merchant"

  return (
    <Tabs
      value={tab}
      onValueChange={(next) => {
        const updated = new URLSearchParams(params)
        if (!next || next === "merchant") updated.delete("tab")
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
          <TabsTrigger value="merchant" className={LINE_TAB}>
            Merchant
          </TabsTrigger>
          <TabsTrigger value="team" className={LINE_TAB}>
            Team
          </TabsTrigger>
          <TabsTrigger value="alerts" className={LINE_TAB}>
            Alerts
          </TabsTrigger>
          <TabsTrigger value="providers" className={LINE_TAB}>
            Payment providers
          </TabsTrigger>
          <TabsTrigger value="api-keys" className={LINE_TAB}>
            API keys
          </TabsTrigger>
          <TabsTrigger value="customer-controls" className={LINE_TAB}>
            Customer controls
          </TabsTrigger>
        </TabsList>
      </div>
      <TabsContent value="merchant">
        <MerchantSettingsTab />
      </TabsContent>
      <TabsContent value="team">
        <TeamTab />
      </TabsContent>
      <TabsContent value="alerts">
        <AlertsTab />
      </TabsContent>
      <TabsContent value="providers">
        <ProvidersTab />
      </TabsContent>
      <TabsContent value="api-keys">
        <ApiKeysTab />
      </TabsContent>
      <TabsContent value="customer-controls">
        <CustomerControlsTab />
      </TabsContent>
    </Tabs>
  )
}

function MerchantSettingsTab() {
  const { data, isPending: loading } = useQuery(
    adminQueries.merchantSettings("Load settings")
  )
  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>
  return (
    <div className="grid gap-10">
      <MerchantProfileForm initial={data?.profile} />
      <RepriceNoticeWindowForm initial={data?.reprice_notice_window_days} />
    </div>
  )
}

function MerchantProfileForm({
  initial,
}: {
  initial?: {
    display_name?: string
    from_email?: string
    support_url?: string
    logo_url?: string
  }
}) {
  const [editing, setEditing] = React.useState(false)
  const queryClient = useQueryClient()
  const updateSettings = useMutation(
    adminMutations.updateMerchantSettings(queryClient)
  )
  const form = useForm({
    defaultValues: {
      displayName: initial?.display_name ?? "",
      fromEmail: initial?.from_email ?? "",
      supportURL: initial?.support_url ?? "",
      logoURL: initial?.logo_url ?? "",
    },
    onSubmit: async ({ value }) => {
      try {
        await updateSettings.mutateAsync({
          profile: {
            display_name: value.displayName || undefined,
            from_email: value.fromEmail || undefined,
            support_url: value.supportURL || undefined,
            logo_url: value.logoURL || undefined,
          },
        })
        form.reset(value)
        toast.success("Profile saved")
        setEditing(false)
      } catch (err) {
        toastApiError(err, "Save profile")
      }
    },
  })

  return (
    <section className="grid gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid gap-1">
          <h2 className="text-base font-semibold">Merchant profile</h2>
          <p className="text-sm text-pretty text-muted-foreground">
            Customer-facing merchant details used on invoices and emails.
          </p>
        </div>
        {editing ? (
          <form.Subscribe selector={(state) => state.isSubmitting}>
            {(isSubmitting) => (
              <div className="flex items-center gap-2">
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={isSubmitting}
                  onClick={() => {
                    form.reset()
                    setEditing(false)
                  }}
                >
                  Cancel
                </Button>
                <Button
                  type="submit"
                  size="sm"
                  form="merchant-profile-form"
                  disabled={isSubmitting}
                >
                  {isSubmitting ? "Saving…" : "Save"}
                </Button>
              </div>
            )}
          </form.Subscribe>
        ) : (
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => setEditing(true)}
          >
            Edit
          </Button>
        )}
      </div>

      {editing ? (
        <form
          id="merchant-profile-form"
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            void form.handleSubmit()
          }}
          className="grid gap-4"
        >
          <form.Field name="displayName">
            {(field) => (
              <SettingEditField label="Display name" id="s-name">
                <Input
                  id="s-name"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  autoComplete="organization"
                />
              </SettingEditField>
            )}
          </form.Field>
          <form.Field name="fromEmail">
            {(field) => (
              <SettingEditField label="From email" id="s-email">
                <Input
                  id="s-email"
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
              <SettingEditField label="Support URL" id="s-support">
                <Input
                  id="s-support"
                  type="url"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  placeholder="https://example.com/support"
                />
              </SettingEditField>
            )}
          </form.Field>
          <form.Field name="logoURL">
            {(field) => (
              <SettingEditField label="Logo URL" id="s-logo">
                <Input
                  id="s-logo"
                  type="url"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  placeholder="https://example.com/logo.png"
                />
              </SettingEditField>
            )}
          </form.Field>
        </form>
      ) : (
        <form.Subscribe selector={(state) => state.values}>
          {(profile) => (
            <dl className="grid gap-4">
              <SettingDetail label="Display name" value={profile.displayName} />
              <SettingDetail label="From email" value={profile.fromEmail} />
              <SettingDetail label="Support URL" value={profile.supportURL} />
              <SettingDetail label="Logo URL" value={profile.logoURL} />
            </dl>
          )}
        </form.Subscribe>
      )}
    </section>
  )
}

// RepriceNoticeWindowForm (#781): the merchant-configurable minimum advance
// notice (days) a subscription price INCREASE must give existing
// subscribers. The catalog price-change wizard reads this same value
// (GET /v1/merchant/settings) for its own date-picker gate; the API enforces
// it regardless of what the console shows.
function RepriceNoticeWindowForm({ initial }: { initial?: number }) {
  const [editing, setEditing] = React.useState(false)
  const queryClient = useQueryClient()
  const updateSettings = useMutation(
    adminMutations.updateMerchantSettings(queryClient)
  )
  const form = useForm({
    defaultValues: { days: String(initial ?? 30) },
    onSubmit: async ({ value }) => {
      try {
        await updateSettings.mutateAsync({
          reprice_notice_window_days: Number(value.days),
        })
        form.reset(value)
        toast.success("Notice window saved")
        setEditing(false)
      } catch (err) {
        toastApiError(err, "Save notice window")
      }
    },
  })

  return (
    <section className="grid gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid max-w-2xl gap-1">
          <h2 className="text-base font-semibold">Pricing policies</h2>
          <p className="text-sm text-pretty text-muted-foreground">
            Set how much notice customers receive before a price increase.
          </p>
        </div>
        {editing ? (
          <form.Subscribe
            selector={(state) => [state.canSubmit, state.isSubmitting]}
          >
            {([canSubmit, isSubmitting]) => (
              <div className="flex items-center gap-2">
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={isSubmitting}
                  onClick={() => {
                    form.reset()
                    setEditing(false)
                  }}
                >
                  Cancel
                </Button>
                <Button
                  type="submit"
                  size="sm"
                  form="notice-window-form"
                  disabled={!canSubmit || isSubmitting}
                >
                  {isSubmitting ? "Saving…" : "Save"}
                </Button>
              </div>
            )}
          </form.Subscribe>
        ) : (
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => setEditing(true)}
          >
            Edit
          </Button>
        )}
      </div>

      {editing ? (
        <form
          id="notice-window-form"
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
                label="Notice period (days)"
                id="s-notice-window"
              >
                <div className="grid gap-1.5">
                  <Input
                    id="s-notice-window"
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
        <form.Subscribe selector={(state) => state.values.days}>
          {(days) => (
            <dl>
              <SettingDetail label="Notice period" value={`${days} days`} />
            </dl>
          )}
        </form.Subscribe>
      )}
    </section>
  )
}

function ProvidersTab() {
  const { data, isPending: loading } = useQuery(adminQueries.paymentProviders())
  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>

  const providerDefinitions = data?.provider_definitions ?? []

  return (
    <div>
      <section className="grid gap-5">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="grid gap-1">
            <h2 className="text-base font-semibold">Payment providers</h2>
            <p className="text-sm text-muted-foreground">
              Configure the payment rails this merchant can use.
            </p>
          </div>
          <ProviderDialog providerDefinitions={providerDefinitions} />
        </div>
        {!data?.data?.length ? (
          <p className="py-2 text-sm text-muted-foreground">
            No payment providers configured.
          </p>
        ) : (
          <Table className="min-w-[44rem]">
            <TableHeader>
              <TableRow>
                <TableHead className="text-muted-foreground">
                  Provider
                </TableHead>
                <TableHead className="text-muted-foreground">
                  Environment
                </TableHead>
                <TableHead className="text-muted-foreground">
                  Credentials
                </TableHead>
                <TableHead className="text-muted-foreground">State</TableHead>
                <TableHead className="text-right text-muted-foreground">
                  Action
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.data.map((provider) => (
                <ProviderRow
                  key={provider.id}
                  provider={provider}
                  providerDefinitions={providerDefinitions}
                />
              ))}
            </TableBody>
          </Table>
        )}
      </section>
    </div>
  )
}

// credentialTitle carries or#812's rotation_version alongside the validation
// stamp — a credential's version floor is what every node cuts over to.
function credentialTitle(c: {
  configured: boolean
  last_validated_at?: string
  rotation_version?: number
}) {
  const parts: string[] = []
  if (c.last_validated_at)
    parts.push(`Validated ${formatDate(c.last_validated_at)}`)
  if (c.rotation_version) parts.push(`rotation v${c.rotation_version}`)
  if (parts.length) return parts.join(" ·")
  return c.configured ? "Configured" : "Not configured"
}

function ProviderRow({
  provider,
  providerDefinitions,
}: {
  provider: PaymentProviderConfig
  providerDefinitions: PaymentProviderDefinition[]
}) {
  const queryClient = useQueryClient()
  const archiveProvider = useMutation(
    adminMutations.archivePaymentProvider(queryClient)
  )
  const definition = providerDefinitions.find((d) => d.rail === provider.rail)
  return (
    <TableRow className={provider.archived ? "opacity-60" : undefined}>
      <TableCell className="py-3">
        <div className="grid min-w-0 leading-tight">
          <span className="font-medium">
            {definition?.display_name ?? provider.rail}
          </span>
          <span className="truncate text-xs text-muted-foreground">
            {provider.rail} · {provider.account_id}
          </span>
        </div>
      </TableCell>
      <TableCell className="py-3 capitalize">
        {provider.environment || "Default"}
      </TableCell>
      <TableCell className="py-3">
        {Object.keys(provider.credentials).length > 0 ? (
          <span className="flex flex-wrap gap-1">
            {Object.entries(provider.credentials).map(([name, credential]) => (
              <Badge
                key={name}
                variant="secondary"
                className={
                  credential.configured ? "" : "bg-held-surface text-held"
                }
                title={credentialTitle(credential)}
              >
                {name}
                {!!credential.rotation_version && (
                  <span className="ml-1 opacity-60">
                    v{credential.rotation_version}
                  </span>
                )}
              </Badge>
            ))}
          </span>
        ) : (
          <span className="text-sm text-muted-foreground">None required</span>
        )}
      </TableCell>
      <TableCell className="py-3">
        {provider.archived ? (
          <Badge variant="secondary">archived</Badge>
        ) : provider.drained ? (
          <Badge variant="secondary" className="bg-held-surface text-held">
            draining ({provider.open_obligations})
          </Badge>
        ) : (
          <Badge
            variant="secondary"
            className="bg-settled-surface text-settled"
          >
            active
          </Badge>
        )}
      </TableCell>
      <TableCell className="py-3 text-right">
        {!provider.archived && (
          <div className="flex justify-end gap-2">
            <RotateCredentialsDialog
              provider={provider}
              credentialKeys={
                providerDefinitions.find((d) => d.rail === provider.rail)
                  ?.credential_keys ?? Object.keys(provider.credentials)
              }
            />
            <Button
              variant="outline"
              size="sm"
              disabled={archiveProvider.isPending}
              onClick={async () => {
                try {
                  await archiveProvider.mutateAsync({
                    rail: provider.rail,
                    environment: provider.environment,
                  })
                  toast.success("Provider archived")
                } catch (err) {
                  toastApiError(err, "Archive provider")
                }
              }}
            >
              Archive
            </Button>
          </div>
        )}
      </TableCell>
    </TableRow>
  )
}

// RotateCredentialsDialog is the or#812 rotation flow. Three properties the
// operator needs stated, because all three are real server behaviour:
//
//  1. The NEW credential is live-probed BEFORE anything is written. A failed
//     probe fails the whole rotation — no secret is stored, no version floor
//     moves, and the OLD credential keeps serving unchanged.
//  2. A committed rotation is deployment-wide, not just this node: it raises the
//     credential's version floor on the shared PSP row, and every node refuses
//     to answer a credential read from a cache entry below that floor.
//  3. Plaintext is dropped from browser state the moment it is submitted.
function RotateCredentialsDialog({
  provider,
  credentialKeys,
}: {
  provider: PaymentProviderConfig
  credentialKeys: string[]
}) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const saveProvider = useMutation(
    adminMutations.savePaymentProvider(queryClient)
  )
  const form = useForm({
    defaultValues: { credentials: {} as Record<string, string> },
    onSubmit: async ({ value }) => {
      const supplied = Object.fromEntries(
        Object.entries(value.credentials).filter(([, item]) => item.trim())
      )
      // Plaintext must not remain in browser state while the request is in flight.
      form.reset()
      try {
        await saveProvider.mutateAsync({
          rail: provider.rail,
          provider: {
            account_id: provider.account_id,
            credentials: supplied,
          },
        })
        toast.success(
          `Credentials validated and rotated. Every node serves the new ${provider.rail} credential from its next read.`
        )
        setOpen(false)
      } catch (err) {
        toastApiError(
          err,
          "Rotation refused. Your current credential is unchanged and still working."
        )
      }
    },
  })

  const close = (next: boolean) => {
    // Never leave plaintext in state behind a closed dialog.
    if (!next) form.reset()
    setOpen(next)
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            Rotate
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            Rotate {provider.rail} credentials · {provider.account_id}
          </DialogTitle>
          <DialogDescription>
            The new credential is validated against the live provider before it
            is stored. If that check fails, nothing is written and the current
            credential keeps serving. Leave a field blank to keep the credential
            it holds now.
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
                  const current = provider.credentials[name]
                  return (
                    <Field
                      key={name}
                      label={name}
                      id={`rot-${provider.id}-${name}`}
                    >
                      <Input
                        id={`rot-${provider.id}-${name}`}
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
                      <p className="text-xs text-muted-foreground">
                        {current?.rotation_version
                          ? `current rotation v${current.rotation_version}`
                          : "no rotation recorded"}
                        {current?.last_validated_at &&
                          ` · last validated ${formatDate(current.last_validated_at)}`}
                      </p>
                    </Field>
                  )
                })}
                <p className="text-xs text-muted-foreground">
                  Once this succeeds, the new credential is used for the next
                  charge and every one after it. Nothing needs restarting, and
                  the old credential stops being used straight away.
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
              {([credentials, isSubmitting]) => (
                <Button
                  type="submit"
                  disabled={
                    isSubmitting ||
                    !Object.values(credentials).some((value) => value.trim())
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

// Where the account ID comes from differs per rail, and the merchant reads it
// off a different dashboard each time. Naming that is the only way this field
// is answerable without a support ticket.
function accountIDHint(rail: string): string | undefined {
  switch (rail) {
    case "nmi":
      return "The Gateway ID shown in your NMI dashboard."
    case "stripe":
      return "Your Stripe account ID. It starts with acct_."
    case "ccbill":
      return "Your account and sub-account joined with a dash, such as 999999-0000."
    case "solana":
      return "Taken from your signing key, so whatever you enter here is ignored."
    default:
      return undefined
  }
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

function ProviderDialog({
  providerDefinitions,
}: {
  providerDefinitions: PaymentProviderDefinition[]
}) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const saveProvider = useMutation(
    adminMutations.savePaymentProvider(queryClient)
  )
  const form = useForm({
    defaultValues: {
      rail: "",
      accountID: "",
      credentials: {} as Record<string, string>,
    },
    onSubmit: async ({ value }) => {
      const credentials = Object.fromEntries(
        Object.entries(value.credentials).filter(([, item]) => item !== "")
      )
      try {
        await saveProvider.mutateAsync({
          rail: value.rail,
          provider: {
            account_id: value.accountID.trim(),
            ...(Object.keys(credentials).length ? { credentials } : {}),
          },
        })
        form.reset()
        toast.success("Provider saved")
        setOpen(false)
      } catch (err) {
        toastApiError(err, "Save provider")
      }
    },
  })

  const handleOpenChange = (next: boolean) => {
    if (!next) form.reset()
    setOpen(next)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger render={<Button size="sm">Configure provider</Button>} />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>Configure payment provider</DialogTitle>
          <DialogDescription>
            Connect the account that will take money for you. Credentials go
            straight into the secret store and are never shown again.
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
                <Field label="Payment rail" id="pv-rail">
                  <Select
                    items={providerDefinitions.map((provider) => ({
                      value: provider.rail,
                      label: railProviderLabel(
                        provider.rail,
                        provider.display_name
                      ),
                    }))}
                    value={field.state.value || null}
                    onValueChange={(value) => {
                      field.handleChange(value ?? "")
                      // Each rail asks for different credentials, so anything
                      // typed for the previous one is not carried over.
                      form.setFieldValue("credentials", {})
                    }}
                  >
                    <SelectTrigger
                      id="pv-rail"
                      className="w-full"
                      aria-invalid={field.state.meta.errors.length > 0}
                    >
                      <SelectValue placeholder="Pick a rail…" />
                    </SelectTrigger>
                    <SelectContent>
                      {providerDefinitions.map((provider) => (
                        <SelectItem key={provider.rail} value={provider.rail}>
                          {railProviderLabel(
                            provider.rail,
                            provider.display_name
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
              {(rail) => (
                <form.Field
                  name="accountID"
                  validators={{
                    onChange: ({ value }) =>
                      value.trim()
                        ? undefined
                        : "Enter the provider account ID",
                  }}
                >
                  {(field) => (
                    <Field
                      label="Account ID"
                      id="pv-acct"
                      hint={accountIDHint(rail)}
                    >
                      <Input
                        id="pv-acct"
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(event) =>
                          field.handleChange(event.target.value)
                        }
                        aria-invalid={field.state.meta.errors.length > 0}
                      />
                      <FormFieldErrors errors={field.state.meta.errors} />
                    </Field>
                  )}
                </form.Field>
              )}
            </form.Subscribe>
            <form.Subscribe selector={(state) => state.values.rail}>
              {(rail) => {
                const selectedProvider = providerDefinitions.find(
                  (provider) => provider.rail === rail
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
                              id={`pv-credential-${name}`}
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
                  state.values.accountID,
                  state.canSubmit,
                  state.isSubmitting,
                ] as const
              }
            >
              {([rail, accountID, canSubmit, isSubmitting]) => (
                <>
                  <Button
                    type="button"
                    variant="outline"
                    onClick={() => handleOpenChange(false)}
                  >
                    Cancel
                  </Button>
                  <Button
                    type="submit"
                    disabled={
                      !rail || !accountID.trim() || !canSubmit || isSubmitting
                    }
                  >
                    {isSubmitting ? "Saving…" : "Save provider"}
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
  const [result, setResult] = React.useState<{
    customerID: string
    currency: string
    creditLimit: number
    trustLevel: string
  }>()
  const lookupControls = useMutation(adminMutations.lookupCustomerControls())
  const updateCreditLimit = useMutation(adminMutations.setCreditLimit())
  const creditForm = useForm({
    defaultValues: { newLimit: "" },
    onSubmit: async ({ value }) => {
      if (!result) return
      const amount = microsFromInput(value.newLimit)
      if (amount === null || amount < 0) return
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
        const { credit, trust } = await lookupControls.mutateAsync({
          customerId: customerID,
          currency,
        })
        setResult({
          customerID,
          currency: credit.currency || currency,
          creditLimit: credit.credit_limit_amount,
          trustLevel: trust.trust_level,
        })
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
          <h2 className="text-base font-semibold">Customer lookup</h2>
          <p className="text-sm text-muted-foreground">
            Find a customer by ID and currency.
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
              <Field label="Customer ID" id="customer-controls-id">
                <Input
                  id="customer-controls-id"
                  placeholder="Customer UUID"
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
              <Field label="Currency" id="customer-controls-currency">
                <Input
                  id="customer-controls-currency"
                  placeholder="USD"
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
            <h2 className="text-base font-semibold">Credit and trust</h2>
            <p className="text-sm text-muted-foreground">
              <span className="font-mono text-xs break-all text-foreground">
                {result.customerID}
              </span>{" "}
              · {result.currency.toUpperCase()}
            </p>
          </div>
          <dl className="grid gap-4">
            <SettingDetail
              label="Trust level"
              value={result.trustLevel || "Default"}
            />
            <SettingDetail
              label="Credit limit"
              value={
                result.creditLimit
                  ? formatMicros(result.creditLimit, result.currency)
                  : "Off"
              }
            />
          </dl>
          <form
            onSubmit={(event) => {
              event.preventDefault()
              event.stopPropagation()
              void creditForm.handleSubmit()
            }}
          >
            <SettingEditField
              label={`New limit (${result.currency.toUpperCase()})`}
              id="customer-controls-limit"
            >
              <creditForm.Field
                name="newLimit"
                validators={{
                  onChange: ({ value }) => {
                    const amount = microsFromInput(value)
                    return value !== "" && amount !== null && amount >= 0
                      ? undefined
                      : "Enter a valid amount"
                  },
                }}
              >
                {(field) => (
                  <div className="grid gap-1.5">
                    <div className="flex gap-2">
                      <Input
                        id="customer-controls-limit"
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
                      Enter 0 to turn credit off.
                    </p>
                  </div>
                )}
              </creditForm.Field>
            </SettingEditField>
          </form>
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
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      {hint ? (
        <p className="text-[13px] text-muted-foreground">{hint}</p>
      ) : null}
      {children}
    </div>
  )
}

function SettingDetail({ label, value }: { label: string; value?: string }) {
  return (
    <div className="grid gap-1.5 md:grid-cols-[11rem_minmax(0,1fr)] md:gap-6">
      <dt className="text-sm text-muted-foreground">{label}</dt>
      <dd
        className={
          value
            ? "min-w-0 text-sm break-words"
            : "text-sm text-muted-foreground"
        }
      >
        {value || "Not set"}
      </dd>
    </div>
  )
}

function SettingEditField({
  label,
  id,
  children,
}: {
  label: string
  id: string
  children: React.ReactNode
}) {
  return (
    <div className="grid gap-2 md:grid-cols-[11rem_minmax(0,1fr)] md:items-center md:gap-6">
      <Label htmlFor={id}>{label}</Label>
      <div className="min-w-0">{children}</div>
    </div>
  )
}
