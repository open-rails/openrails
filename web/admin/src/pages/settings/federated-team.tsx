import * as React from "react"
import { toast } from "sonner"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
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
import { api } from "@/lib/api/client"
import type { FederatedGrant, ListPage } from "@/lib/api/generated/wire"
import { formatDate } from "@/lib/format"
import { merchantQueryKeys } from "@/lib/queries"
import { toastApiError } from "@/lib/toast"

const ROLES = ["viewer", "support", "owner"] as const

// FederatedTeamTab is the team of a merchant whose staff sign in at a
// trusted issuer: the issuer's own permissions, plus roles owners grant here
// by email. The invitee accepts after signing in with that verified email.
export function FederatedTeamTab() {
  const queryClient = useQueryClient()
  const key = [...merchantQueryKeys().team(), "federated"]
  const grants = useQuery({
    queryKey: key,
    queryFn: () =>
      api<ListPage<FederatedGrant>>("/merchant/federated-grants").then(
        (page) => page.data
      ),
    meta: { errorAction: "Load team" },
  })
  const [email, setEmail] = React.useState("")
  const [role, setRole] = React.useState<string>("viewer")
  const invite = useMutation({
    mutationFn: () =>
      api<FederatedGrant>("/merchant/federated-grants", {
        method: "POST",
        body: { email, role },
      }),
    onSuccess: (grant) => {
      setEmail("")
      toast.success(`Invited ${grant.email}`)
      return queryClient.invalidateQueries({ queryKey: key })
    },
    onError: (error) => toastApiError(error, "Invite"),
  })
  const revoke = useMutation({
    mutationFn: (id: string) =>
      api(`/merchant/federated-grants/${encodeURIComponent(id)}`, {
        method: "DELETE",
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: key }),
    onError: (error) => toastApiError(error, "Revoke access"),
  })

  return (
    <section className="grid gap-5">
      <div className="grid gap-1">
        <h2 className="text-base font-semibold">Team</h2>
        <p className="text-sm text-muted-foreground">
          People your identity provider grants access to have it already. Invite
          anyone else by email; they accept after signing in with it.
        </p>
      </div>
      <form
        className="flex flex-wrap items-end gap-3"
        onSubmit={(event) => {
          event.preventDefault()
          if (email.trim()) invite.mutate()
        }}
      >
        <div className="grid min-w-64 flex-1 gap-1.5">
          <Label htmlFor="federated-invite-email">Email</Label>
          <Input
            id="federated-invite-email"
            type="email"
            value={email}
            onChange={(event) => setEmail(event.target.value)}
            placeholder="teammate@example.com"
          />
        </div>
        <div className="grid w-40 gap-1.5">
          <Label>Role</Label>
          <Select
            value={role}
            onValueChange={(value) => value && setRole(value)}
          >
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {ROLES.map((value) => (
                <SelectItem key={value} value={value}>
                  {value}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <Button type="submit" disabled={invite.isPending || !email.trim()}>
          Invite
        </Button>
      </form>
      {grants.isPending ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : (grants.data ?? []).length === 0 ? (
        <p className="text-sm text-muted-foreground">No invitations yet.</p>
      ) : (
        <Table className="min-w-[36rem]">
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead className="text-muted-foreground">Email</TableHead>
              <TableHead className="w-28 text-muted-foreground">Role</TableHead>
              <TableHead className="w-36 text-muted-foreground">
                Status
              </TableHead>
              <TableHead className="w-24 text-right text-muted-foreground">
                Action
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {(grants.data ?? []).map((grant) => (
              <TableRow key={grant.id}>
                <TableCell>{grant.email}</TableCell>
                <TableCell>{grant.role}</TableCell>
                <TableCell>
                  {grant.accepted_at ? (
                    <span className="text-sm">
                      Since {formatDate(grant.accepted_at)}
                    </span>
                  ) : (
                    <Badge variant="secondary">Pending</Badge>
                  )}
                </TableCell>
                <TableCell className="text-right">
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={revoke.isPending}
                    onClick={() => revoke.mutate(grant.id)}
                  >
                    Revoke
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  )
}
