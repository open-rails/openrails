import * as React from "react"
import { toast } from "sonner"
import { useQuery, useQueryClient } from "@tanstack/react-query"

import { TypedConfirmDialog } from "@/components/typed-confirm-dialog"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { Badge } from "@/components/ui/badge"
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
import {
  changeTeamRole,
  inviteTeamMember,
  removeTeamMember,
  revokeTeamInvite,
} from "@/lib/api/endpoints"
import type { TeamInvite, TeamInviteResult, TeamMember } from "@/lib/api/types"
import { cn } from "@/lib/utils"
import { formatDate } from "@/lib/format"
import { toastApiError } from "@/lib/toast"
import { adminQueries, queryKeys } from "@/lib/queries"

// Fixed merchant catalog roles (#567) a teammate can hold, least privilege
// first. The API validates against the same catalog; these descriptions are the
// plain-language contract for team members (distinct from an API key's).
const ROLES: { value: string; label: string; description: string }[] = [
  {
    value: "viewer",
    label: "Viewer — read-only",
    description:
      "Can view metrics, payments, subscriptions, catalog, and settings. Cannot " +
      "change anything or move money. For finance, audit, and analyst access.",
  },
  {
    value: "support",
    label: "Support — customer operations",
    description:
      "Everything Viewer can, plus customer fixes: refunds, subscription changes, " +
      "and entitlement grants. Cannot change settings, catalog, providers, keys, or the team.",
  },
  {
    value: "owner",
    label: "Owner — full control",
    description:
      "Full authority over this merchant, including settings, payment providers, " +
      "API keys, and the team itself. Only owners can manage teammates.",
  },
]

function roleLabel(role: string): string {
  return ROLES.find((r) => r.value === role)?.label.split(" — ")[0] ?? role
}

export function TeamTab() {
  const queryClient = useQueryClient()
  const members = useQuery(adminQueries.team())
  const invites = useQuery(adminQueries.teamInvites())

  const reload = () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.team() })
  }

  if (members.isPending)
    return <p className="text-sm text-muted-foreground">Loading…</p>

  const team = members.data?.data ?? []
  const pending = (invites.data?.data ?? []).filter(
    (i) => !i.redeemed_at && !i.revoked_at
  )
  const ownerCount = team.filter((m) => m.role === "owner").length

  return (
    <div className="flex flex-col gap-10">
      <section className="grid gap-5">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="grid gap-1">
            <h2 className="text-base font-semibold">Team members</h2>
            <p className="text-sm text-muted-foreground">
              Manage access and roles. At least one owner is required.
            </p>
          </div>
          <InviteDialog
            invitesEnabled={invites.data?.invites_enabled ?? false}
            onDone={reload}
          />
        </div>
        {team.length === 0 ? (
          <p className="text-sm text-muted-foreground">No team members yet.</p>
        ) : (
          <Table className="min-w-[36rem]">
            <TableHeader>
              <TableRow>
                <TableHead className="text-muted-foreground">Member</TableHead>
                <TableHead className="w-40 text-muted-foreground">
                  Role
                </TableHead>
                <TableHead className="w-24 text-right text-muted-foreground">
                  Action
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {team.map((m) => (
                <MemberRow
                  key={m.user_id}
                  member={m}
                  isLastOwner={m.role === "owner" && ownerCount <= 1}
                  onDone={reload}
                />
              ))}
            </TableBody>
          </Table>
        )}
      </section>

      {pending.length > 0 && (
        <section className="grid gap-5">
          <div className="grid gap-1">
            <h2 className="text-base font-semibold">Pending invites</h2>
            <p className="text-sm text-muted-foreground">
              Invitations awaiting acceptance.
            </p>
          </div>
          <Table className="min-w-[36rem]">
            <TableHeader>
              <TableRow>
                <TableHead className="text-muted-foreground">Role</TableHead>
                <TableHead className="text-muted-foreground">Created</TableHead>
                <TableHead className="text-muted-foreground">Expires</TableHead>
                <TableHead className="w-24 text-right text-muted-foreground">
                  Action
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {pending.map((inv) => (
                <InviteRow key={inv.id} invite={inv} onDone={reload} />
              ))}
            </TableBody>
          </Table>
        </section>
      )}
    </div>
  )
}

function MemberRow({
  member,
  isLastOwner,
  onDone,
}: {
  member: TeamMember
  isLastOwner: boolean
  onDone: () => void
}) {
  const [confirmOpen, setConfirmOpen] = React.useState(false)
  const label = member.email || member.username || member.user_id
  const initials = (member.username || member.email || "Member")
    .slice(0, 2)
    .toUpperCase()
  return (
    <TableRow>
      <TableCell className="py-3">
        <div className="flex items-center gap-3">
          <Avatar className="size-8 rounded-lg">
            <AvatarFallback className="rounded-lg text-xs">
              {initials}
            </AvatarFallback>
          </Avatar>
          <div className="grid min-w-0 leading-tight">
            <span className="truncate font-medium">{label}</span>
            {member.username && member.email && (
              <span className="truncate text-xs text-muted-foreground">
                @{member.username}
              </span>
            )}
          </div>
        </div>
      </TableCell>
      <TableCell className="py-3">
        <RoleSelect
          value={member.role}
          disabled={isLastOwner}
          disabledTitle={
            isLastOwner ? "The last owner cannot be demoted" : undefined
          }
          onChange={async (role) => {
            try {
              await changeTeamRole(member.user_id, role)
              toast.success(`${label} is now ${roleLabel(role)}`)
              onDone()
            } catch (err) {
              toastApiError(err, "Change role")
            }
          }}
        />
      </TableCell>
      <TableCell className="py-3 text-right">
        <Button
          variant="destructive"
          size="sm"
          disabled={isLastOwner}
          title={isLastOwner ? "The last owner cannot be removed" : undefined}
          onClick={() => setConfirmOpen(true)}
        >
          Remove
        </Button>
        <TypedConfirmDialog
          open={confirmOpen}
          onOpenChange={setConfirmOpen}
          title={`Remove ${label}?`}
          description="They will immediately lose access to this merchant console. This cannot be undone."
          confirmationWord="REMOVE"
          actionLabel="Remove member"
          onConfirm={async () => {
            try {
              await removeTeamMember(member.user_id)
              toast.success(`${label} removed`)
              onDone()
            } catch (err) {
              toastApiError(err, "Remove member")
            }
          }}
        />
      </TableCell>
    </TableRow>
  )
}

function RoleSelect({
  value,
  disabled,
  disabledTitle,
  onChange,
}: {
  value: string
  disabled?: boolean
  disabledTitle?: string
  onChange: (role: string) => void
}) {
  return (
    <Select
      value={value}
      disabled={disabled}
      onValueChange={(next) => {
        if (next && next !== value) onChange(next)
      }}
    >
      <SelectTrigger className="w-[132px]" title={disabledTitle}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {ROLES.map((r) => (
          <SelectItem key={r.value} value={r.value}>
            {roleLabel(r.value)}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

function InviteRow({
  invite,
  onDone,
}: {
  invite: TeamInvite
  onDone: () => void
}) {
  const [busy, setBusy] = React.useState(false)
  return (
    <TableRow>
      <TableCell className="py-3">
        <Badge variant="secondary">{roleLabel(invite.role)}</Badge>
      </TableCell>
      <TableCell className="py-3 text-muted-foreground">
        {formatDate(invite.created_at)}
      </TableCell>
      <TableCell className="py-3 text-muted-foreground">
        {invite.expires_at ? formatDate(invite.expires_at) : "never"}
      </TableCell>
      <TableCell className="py-3 text-right">
        <Button
          variant="destructive"
          size="sm"
          disabled={busy}
          onClick={async () => {
            setBusy(true)
            try {
              await revokeTeamInvite(invite.id)
              toast.success("Invite revoked")
              onDone()
            } catch (err) {
              toastApiError(err, "Revoke invite")
            } finally {
              setBusy(false)
            }
          }}
        >
          Revoke
        </Button>
      </TableCell>
    </TableRow>
  )
}

function InviteDialog({
  invitesEnabled,
  onDone,
}: {
  invitesEnabled: boolean
  onDone: () => void
}) {
  const [open, setOpen] = React.useState(false)
  const [email, setEmail] = React.useState("")
  const [role, setRole] = React.useState("viewer")
  const [busy, setBusy] = React.useState(false)
  const [minted, setMinted] = React.useState<TeamInviteResult | null>(null)

  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (!next) {
      setEmail("")
      setRole("viewer")
      setMinted(null)
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger render={<Button size="sm">Invite member</Button>} />
      <DialogContent>
        {minted?.url ? (
          <ShowInviteLink
            result={minted}
            onClose={() => handleOpenChange(false)}
          />
        ) : (
          <>
            <DialogHeader>
              <DialogTitle>Invite a teammate</DialogTitle>
              <DialogDescription>
                If the email already has an account, they&apos;re added to the
                team right away.
                {invitesEnabled
                  ? " Otherwise you get a single-use link to send them."
                  : " New emails without an account can't be invited on this deployment — the account must be provisioned first."}
              </DialogDescription>
            </DialogHeader>
            <div className="grid gap-3">
              <div className="grid gap-1.5">
                <Label htmlFor="team-email">Email</Label>
                <Input
                  id="team-email"
                  type="email"
                  placeholder="teammate@example.com"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                />
              </div>
              <div className="grid gap-1.5">
                <Label>Role</Label>
                <div className="grid gap-2">
                  {ROLES.map((r) => (
                    <button
                      key={r.value}
                      type="button"
                      onClick={() => setRole(r.value)}
                      aria-pressed={role === r.value}
                      className={cn(
                        "rounded-md border p-3 text-left transition-colors",
                        role === r.value
                          ? "border-primary bg-primary/5"
                          : "hover:bg-muted/50"
                      )}
                    >
                      <p className="text-sm font-medium">{r.label}</p>
                      <p className="mt-1 text-xs text-muted-foreground">
                        {r.description}
                      </p>
                    </button>
                  ))}
                </div>
              </div>
            </div>
            <DialogFooter>
              <Button
                disabled={busy || !email.trim()}
                onClick={async () => {
                  setBusy(true)
                  try {
                    const result = await inviteTeamMember(email.trim(), role)
                    if (result.added) {
                      toast.success(
                        `${result.member?.email || email.trim()} added to the team`
                      )
                      onDone()
                      handleOpenChange(false)
                    } else {
                      // A link was minted — show it once for the owner to share.
                      setMinted(result)
                      onDone()
                    }
                  } catch (err) {
                    toastApiError(err, "Invite member")
                  } finally {
                    setBusy(false)
                  }
                }}
              >
                {busy ? "Inviting…" : "Send invite"}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}

function ShowInviteLink({
  result,
  onClose,
}: {
  result: TeamInviteResult
  onClose: () => void
}) {
  const [copied, setCopied] = React.useState(false)
  const url = result.url ?? ""
  return (
    <>
      <DialogHeader>
        <DialogTitle>Invite link created</DialogTitle>
        <DialogDescription>
          Send this single-use link to the invitee. They register with it and
          join as{" "}
          <span className="font-semibold text-foreground">
            {roleLabel(result.invite?.role ?? "")}
          </span>
          .{" "}
          <span className="font-semibold text-foreground">
            You won&apos;t see it again.
          </span>
        </DialogDescription>
      </DialogHeader>
      <div className="flex items-center gap-2">
        <code className="min-w-0 flex-1 overflow-x-auto rounded-md border bg-muted px-3 py-2 text-xs whitespace-nowrap">
          {url}
        </code>
        <Button
          variant="outline"
          size="sm"
          onClick={async () => {
            try {
              await navigator.clipboard.writeText(url)
              setCopied(true)
              toast.success("Invite link copied")
            } catch {
              toast.error("Copy failed — select the link text manually")
            }
          }}
        >
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      <DialogFooter>
        <Button onClick={onClose}>Done</Button>
      </DialogFooter>
    </>
  )
}
