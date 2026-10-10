import * as React from "react"
import { useQuery } from "@tanstack/react-query"

import { Button } from "@/components/ui/button"
import { Label } from "@/components/ui/label"
import { adminQueries } from "@/lib/queries"

// ReadOnlyNotice explains the missing edit controls when the merchant's
// configuration comes from a file.
export function ReadOnlyNotice() {
  const { data } = useQuery(adminQueries.config())
  if (!data || data.capabilities.features?.merchant_config_edits) return null
  return (
    <p className="rounded-md border px-3 py-2 text-sm text-muted-foreground">
      Read-only: this merchant's configuration comes from a file. Change it
      there.
    </p>
  )
}

// EditActions is a section's Edit button, or Cancel and Save while editing.
// Without edits it renders nothing.
export function EditActions({
  edits,
  editing,
  form,
  saving,
  canSave = true,
  onEdit,
  onCancel,
}: {
  edits: boolean
  editing: boolean
  form: string
  saving: boolean
  canSave?: boolean
  onEdit: () => void
  onCancel: () => void
}) {
  if (!edits) return null
  if (!editing)
    return (
      <Button type="button" variant="outline" size="sm" onClick={onEdit}>
        Edit
      </Button>
    )
  return (
    <div className="flex items-center gap-2">
      <Button
        type="button"
        variant="outline"
        size="sm"
        disabled={saving}
        onClick={onCancel}
      >
        Cancel
      </Button>
      <Button type="submit" size="sm" form={form} disabled={!canSave || saving}>
        {saving ? "Saving…" : "Save"}
      </Button>
    </div>
  )
}

export function SettingDetail({
  label,
  value,
}: {
  label: string
  value?: string
}) {
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

export function SettingEditField({
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
