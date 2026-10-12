import * as React from "react"
import { toast } from "sonner"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { APIKeyCreated, GroupRole } from "@openrails/auth-ui/client"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
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
import { TypedConfirmDialog } from "@/components/typed-confirm-dialog"
import { DIALOG_FORM, DIALOG_WIDE } from "@/lib/dialog-width"
import {
  appRoutes,
  catalogOf,
  grantLabel,
  permissionInfo,
  ROLE_NAME,
  roleTitle,
  rolesMutations,
  rolesQueries,
  SECTIONS,
  type RolesAccess,
} from "@/lib/roles"
import { toastApiError } from "@/lib/toast"

// RolesTab lists the roles of one scope staff hold their permissions in, the
// root group's as all-merchants staff, with who holds each, and lets those
// holding roles:manage define their own: least privilege for a program's API
// key or a teammate. Giving a role to a teammate or a new API key needs the
// team's or the keys' own permissions.
export function RolesTab({ access }: { access: RolesAccess }) {
  const roles = useQuery(rolesQueries.list(access.scope))
  const members = useQuery({
    ...rolesQueries.members(access.scope),
    enabled: access.membersRead,
  })
  const keys = useQuery({
    ...rolesQueries.apiKeys(access.scope),
    enabled: access.keysRead,
  })
  const [editing, setEditing] = React.useState<GroupRole | "new" | null>(null)
  const [assigning, setAssigning] = React.useState<GroupRole | null>(null)
  const [deleting, setDeleting] = React.useState<GroupRole | null>(null)
  const queryClient = useQueryClient()
  const remove = useMutation(rolesMutations.remove(queryClient, access.scope))

  if (roles.isPending)
    return <p className="text-sm text-muted-foreground">Loading…</p>
  const list = roles.data ?? []
  const catalog = catalogOf(list, access.persona)
  const assigns = access.membersManage || access.keysManage
  const holders = (role: string) => {
    const parts: string[] = []
    if (members.data) {
      const n = members.data.filter((m) => m.role === role).length
      parts.push(`${n} ${n === 1 ? "member" : "members"}`)
    }
    if (keys.data) {
      const n = keys.data.filter((k) => k.role === role && !k.revoked_at).length
      parts.push(`${n} ${n === 1 ? "key" : "keys"}`)
    }
    return parts.join(" · ")
  }

  return (
    <section className="grid gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid max-w-2xl gap-1">
          <h2 className="text-base font-semibold">
            {access.everyMerchant ? "All-merchants staff" : "Roles"}
          </h2>
          <p className="text-sm text-pretty text-muted-foreground">
            {access.everyMerchant
              ? "Roles held in the root group. Their holders act at every merchant."
              : "A role is a set of permissions held by a teammate or an API key. Define your own so each holds only what it needs: a storefront's key, for example, only checks entitlements and reads products."}
          </p>
        </div>
        {access.manage && (
          <Button onClick={() => setEditing("new")}>New role</Button>
        )}
      </div>
      <Table className="min-w-[44rem]">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead className="text-muted-foreground">Role</TableHead>
            <TableHead className="text-muted-foreground">Grants</TableHead>
            {(access.membersRead || access.keysRead) && (
              <TableHead className="text-muted-foreground">Held by</TableHead>
            )}
            {(assigns || access.manage) && (
              <TableHead className="text-right text-muted-foreground">
                Action
              </TableHead>
            )}
          </TableRow>
        </TableHeader>
        <TableBody>
          {list.map((role) => (
            <TableRow key={role.name}>
              <TableCell className="py-3 align-top">
                <div className="flex items-center gap-2">
                  <span className="font-medium">{roleTitle(role.name)}</span>
                  <Badge variant={role.custom ? "secondary" : "outline"}>
                    {role.custom ? "custom" : "built-in"}
                  </Badge>
                </div>
                <span className="font-mono text-xs text-muted-foreground">
                  {role.name}
                </span>
              </TableCell>
              <TableCell className="py-3 align-top">
                <span className="flex flex-wrap gap-1">
                  {role.grants.map((grant) => (
                    <Badge key={grant} variant="secondary" title={grant}>
                      {grantLabel(grant, access.persona)}
                    </Badge>
                  ))}
                </span>
              </TableCell>
              {(access.membersRead || access.keysRead) && (
                <TableCell className="py-3 align-top text-sm text-muted-foreground">
                  {holders(role.name)}
                </TableCell>
              )}
              {(assigns || access.manage) && (
                <TableCell className="py-3 text-right align-top">
                  <div className="flex justify-end gap-2">
                    {assigns && (
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => setAssigning(role)}
                      >
                        Assign
                      </Button>
                    )}
                    {access.manage && role.custom && (
                      <>
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() => setEditing(role)}
                        >
                          Edit
                        </Button>
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() => setDeleting(role)}
                        >
                          Delete
                        </Button>
                      </>
                    )}
                  </div>
                </TableCell>
              )}
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {editing && (
        <RoleDialog
          access={access}
          catalog={catalog}
          role={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
        />
      )}
      {assigning && (
        <AssignDialog
          access={access}
          role={assigning}
          onClose={() => setAssigning(null)}
        />
      )}
      <TypedConfirmDialog
        open={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={`Delete the ${deleting ? roleTitle(deleting.name) : ""} role?`}
        description="Every teammate holding it loses it, and every API key and invitation carrying it is revoked."
        confirmationWord={deleting ? roleTitle(deleting.name) : ""}
        actionLabel="Delete role"
        onConfirm={async () => {
          if (!deleting) return
          try {
            await remove.mutateAsync(deleting.name)
            toast.success(`${roleTitle(deleting.name)} deleted`)
          } catch (err) {
            toastApiError(err, "Delete role")
          }
        }}
      />
    </section>
  )
}

// RoleDialog creates a custom role or replaces what one grants, from the
// persona's permissions; one the editor does not hold cannot be granted.
function RoleDialog({
  access,
  catalog,
  role,
  onClose,
}: {
  access: RolesAccess
  catalog: string[]
  role: GroupRole | null
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const create = useMutation(rolesMutations.create(queryClient, access.scope))
  const update = useMutation(rolesMutations.update(queryClient, access.scope))
  const [name, setName] = React.useState("")
  const [selected, setSelected] = React.useState<Set<string>>(
    () => new Set(role?.grants ?? [])
  )
  const pending = create.isPending || update.isPending
  const nameValid = role !== null || ROLE_NAME.test(name)
  // A grant outside the catalog (a pattern set through the API) is listed
  // under Other, so an edit can keep or drop it.
  const offered = [...new Set([...catalog, ...(role?.grants ?? [])])]
  const sections = SECTIONS.map((section) => ({
    ...section,
    perms: offered.filter(
      (perm) =>
        (permissionInfo(perm, access.persona)?.section ?? "other") ===
        section.id
    ),
  })).filter((section) => section.perms.length > 0)
  const toggle = (perm: string, on: boolean) =>
    setSelected((current) => {
      const next = new Set(current)
      if (on) next.add(perm)
      else next.delete(perm)
      return next
    })

  const submit = async () => {
    const permissions = [...selected]
    try {
      if (role) {
        await update.mutateAsync({ role: role.name, permissions })
        toast.success(`${roleTitle(role.name)} saved`)
      } else {
        await create.mutateAsync({ name, permissions })
        toast.success(`${name} created`)
      }
      onClose()
    } catch (err) {
      toastApiError(err, role ? "Save role" : "Create role")
    }
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className={DIALOG_WIDE}>
        <DialogHeader>
          <DialogTitle>
            {role ? `Edit ${roleTitle(role.name)}` : "New role"}
          </DialogTitle>
          <DialogDescription>
            {role
              ? "The change applies to everyone holding the role on their next request."
              : "Pick only what its holders need. You can grant only permissions you hold."}
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid max-h-[60vh] gap-5 overflow-y-auto pr-1"
          onSubmit={(event) => {
            event.preventDefault()
            void submit()
          }}
          id="role-form"
        >
          {!role && (
            <div className="grid gap-1.5">
              <Label htmlFor="role-name">Name</Label>
              <Input
                id="role-name"
                value={name}
                maxLength={64}
                placeholder="storefront"
                autoComplete="off"
                onChange={(event) => setName(event.target.value.toLowerCase())}
                aria-invalid={name !== "" && !nameValid}
              />
              <p className="text-xs text-muted-foreground">
                Lowercase letters, digits and dashes. The role is{" "}
                <span className="font-mono">
                  {access.persona}:custom-{name || "…"}
                </span>
                .
              </p>
            </div>
          )}
          {sections.map((section) => (
            <fieldset key={section.id} className="grid gap-2">
              <legend className="text-sm font-semibold">{section.title}</legend>
              <p className="text-xs text-muted-foreground">
                {section.description}
              </p>
              {section.perms.map((perm) => {
                const info = permissionInfo(perm, access.persona)
                const routes = appRoutes(perm)
                const held = access.held.has(perm)
                const id = `perm-${perm.replaceAll(":", "-")}`
                return (
                  <label
                    key={perm}
                    htmlFor={id}
                    className="flex items-start gap-3 rounded-md border p-2.5 has-[:disabled]:opacity-60"
                    title={held ? undefined : "You don't hold this permission"}
                  >
                    <input
                      id={id}
                      type="checkbox"
                      className="mt-0.5 size-4 accent-primary"
                      checked={selected.has(perm)}
                      disabled={!held && !selected.has(perm)}
                      onChange={(event) => toggle(perm, event.target.checked)}
                    />
                    <span className="grid min-w-0 gap-0.5">
                      <span className="text-sm">{info?.label ?? perm}</span>
                      <span className="font-mono text-xs text-muted-foreground">
                        {perm}
                      </span>
                      {routes.map((route) => (
                        <span
                          key={`${route.method} ${route.path}`}
                          className="font-mono text-xs text-muted-foreground"
                        >
                          {route.method} {route.path}
                        </span>
                      ))}
                    </span>
                  </label>
                )
              })}
            </fieldset>
          ))}
        </form>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={pending}>
            Cancel
          </Button>
          <Button
            type="submit"
            form="role-form"
            disabled={pending || !nameValid || selected.size === 0}
          >
            {role ? "Save role" : "Create role"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// AssignDialog gives a role to a teammate, replacing the one they hold, or
// to a new API key, whose secret is shown once.
function AssignDialog({
  access,
  role,
  onClose,
}: {
  access: RolesAccess
  role: GroupRole
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const members = useQuery({
    ...rolesQueries.members(access.scope),
    enabled: access.membersManage,
  })
  const setRole = useMutation(
    rolesMutations.setMemberRole(queryClient, access.scope)
  )
  const createKey = useMutation(
    rolesMutations.createAPIKey(queryClient, access.scope)
  )
  const [member, setMember] = React.useState<string | null>(null)
  const [keyName, setKeyName] = React.useState("")
  const [created, setCreated] = React.useState<APIKeyCreated | null>(null)
  const title = roleTitle(role.name)
  const candidates = (members.data ?? []).filter((m) => m.role !== role.name)
  const memberName = (id: string) => {
    const m = members.data?.find((m) => m.subject.id === id)
    return m?.user?.username ?? id
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>Assign {title}</DialogTitle>
          <DialogDescription>
            Give this role to a teammate or to a new API key for a program.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-6">
          {access.membersManage && (
            <form
              className="grid gap-2"
              onSubmit={(event) => {
                event.preventDefault()
                if (!member) return
                setRole
                  .mutateAsync({ userId: member, role: role.name })
                  .then(() => {
                    toast.success(`${memberName(member)} now holds ${title}`)
                    setMember(null)
                  })
                  .catch((err: unknown) => toastApiError(err, "Assign role"))
              }}
            >
              <Label htmlFor="assign-member">Team member</Label>
              <div className="flex flex-wrap gap-2">
                <Select
                  items={candidates.map((m) => ({
                    value: m.subject.id,
                    label: `${m.user?.username ?? m.subject.id} (${roleTitle(m.role)})`,
                  }))}
                  value={member}
                  onValueChange={(value) => setMember(value ?? null)}
                >
                  <SelectTrigger id="assign-member" className="min-w-56 flex-1">
                    <SelectValue placeholder="Pick a teammate…" />
                  </SelectTrigger>
                  <SelectContent>
                    {candidates.map((m) => (
                      <SelectItem key={m.subject.id} value={m.subject.id}>
                        {m.user?.username ?? m.subject.id} ({roleTitle(m.role)})
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <Button type="submit" disabled={!member || setRole.isPending}>
                  Give role
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">
                It replaces the role they hold.
              </p>
            </form>
          )}
          {access.keysManage && (
            <form
              className="grid gap-2"
              onSubmit={(event) => {
                event.preventDefault()
                createKey
                  .mutateAsync({ name: keyName.trim(), role: role.name })
                  .then((key) => {
                    setCreated(key)
                    setKeyName("")
                  })
                  .catch((err: unknown) => toastApiError(err, "Create API key"))
              }}
            >
              <Label htmlFor="assign-key">New API key</Label>
              <div className="flex flex-wrap gap-2">
                <Input
                  id="assign-key"
                  className="min-w-56 flex-1"
                  required
                  maxLength={120}
                  placeholder="storefront-prod"
                  value={keyName}
                  onChange={(event) => setKeyName(event.target.value)}
                />
                <Button
                  type="submit"
                  disabled={!keyName.trim() || createKey.isPending}
                >
                  Create key
                </Button>
              </div>
              {created && (
                <div role="status" className="grid gap-1.5">
                  <p className="text-sm">
                    Copy the secret of {created.api_key.name} now; it is never
                    shown again.
                  </p>
                  <Input
                    readOnly
                    aria-label="API key secret"
                    className="font-mono text-xs"
                    value={created.secret}
                    onFocus={(event) => event.target.select()}
                  />
                </div>
              )}
            </form>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Done
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
