import { ApiError, authApi } from "@/lib/api/client"

export interface AccountRecovery {
  token: string
  expires_at: string
  purge_at: string
}

export interface RecoveryState {
  recovery?: AccountRecovery
  notice?: string
  pending?: boolean
}

const invalidRecovery =
  "This recovery confirmation is invalid or expired. Sign in again."

function readRecovery(value: unknown): RecoveryState {
  if (!value || typeof value !== "object") return { notice: invalidRecovery }
  const record = value as Record<string, unknown>
  const { token, expires_at, purge_at } = record
  if (
    typeof token !== "string" ||
    !/^[A-Za-z0-9_-]{32,256}$/.test(token) ||
    typeof expires_at !== "string" ||
    typeof purge_at !== "string"
  )
    return { notice: invalidRecovery }
  const expires = Date.parse(expires_at)
  const purge = Date.parse(purge_at)
  if (
    !Number.isFinite(expires) ||
    !Number.isFinite(purge) ||
    expires <= Date.now() ||
    expires > purge
  ) {
    return { notice: invalidRecovery }
  }
  return { recovery: { token, expires_at, purge_at } }
}

// Move the proof out of the API error before React Query retains that error.
// It belongs only to the current confirmation screen, never session storage.
export function takeAccountRecovery(error: unknown): RecoveryState | undefined {
  if (
    !(error instanceof ApiError) ||
    error.status !== 409 ||
    error.code !== "account_recovery_required"
  )
    return
  const value = error.metadata?.recovery
  if (error.metadata) delete error.metadata.recovery
  return readRecovery(value)
}

export function readRecoveryFragment(): RecoveryState | undefined {
  if (typeof window === "undefined") return
  const params = new URLSearchParams(window.location.hash.slice(1))
  if (params.get("error") !== "account_recovery_required") return
  try {
    return readRecovery(JSON.parse(params.get("recovery") ?? "null"))
  } catch {
    return { notice: invalidRecovery }
  }
}

export function clearRecoveryFragment() {
  const params = new URLSearchParams(window.location.hash.slice(1))
  if (params.get("error") === "account_recovery_required") {
    history.replaceState(
      null,
      "",
      window.location.pathname + window.location.search
    )
  }
}

export async function confirmAccountRecovery(token: string): Promise<void> {
  await authApi<void>("/account/recovery/confirm", {
    method: "POST",
    body: { token },
  })
}
