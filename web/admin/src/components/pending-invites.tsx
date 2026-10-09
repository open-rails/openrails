import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { Button } from "@/components/ui/button"
import { api } from "@/lib/api/client"
import type {
  FederatedInvite,
  ListPage,
  UserMerchant,
} from "@/lib/api/generated/wire"
import { toastApiError } from "@/lib/toast"

// PendingInvites lists the merchants that invited the signed-in issuer
// user's verified email; accepting one grants its role.
export function PendingInvites() {
  const queryClient = useQueryClient()
  const invites = useQuery({
    queryKey: ["auth", "invites"],
    queryFn: () => api<ListPage<FederatedInvite>>("/merchants/invites"),
    retry: false,
  })
  const accept = useMutation({
    mutationFn: (id: string) =>
      api<UserMerchant>(`/merchants/invites/${encodeURIComponent(id)}/accept`, {
        method: "POST",
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["auth"] }),
    onError: (error) => toastApiError(error, "Accept invite"),
  })
  const pending = invites.data?.data ?? []
  if (pending.length === 0) return null
  return (
    <div className="mt-8 grid gap-3">
      <h2 className="text-sm font-medium">Invitations</h2>
      {pending.map((invite) => (
        <div
          key={invite.id}
          className="flex items-center justify-between gap-4 rounded-md border px-3 py-2"
        >
          <div className="min-w-0">
            <p className="truncate text-sm font-medium">
              {invite.merchant.display_name || invite.merchant.slug}
            </p>
            <p className="text-xs text-muted-foreground">as {invite.role}</p>
          </div>
          <Button
            size="sm"
            disabled={accept.isPending}
            onClick={() => accept.mutate(invite.id)}
          >
            Accept
          </Button>
        </div>
      ))}
    </div>
  )
}
