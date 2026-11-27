// Whether the console's AuthKit runs this deployment's own accounts: it
// serves their people and groups (/capabilities route_groups). Only then do
// the first owner's setup, invitations and the Team page apply.
import { useCapabilities } from "@openrails/auth-ui/react"

export interface OwnAccounts {
  loading: boolean
  error: boolean
  retry: () => void
  team: boolean
  setup: boolean
  invitations: boolean
}

export function useOwnAccounts(): OwnAccounts {
  const { capabilities, loading, error, refetch } = useCapabilities()
  const served = new Set<string>(capabilities?.route_groups ?? [])
  const own = served.has("groups")
  return {
    loading,
    error: !!error,
    retry: refetch,
    team: own,
    setup: own && served.has("setup"),
    invitations: own && served.has("invitations"),
  }
}
