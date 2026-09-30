import { authApi } from "@/lib/api/client"

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

// AccountRecoveryRequired is a sign-in that proved the account and found it
// scheduled for deletion (AuthResult account_recovery_required). It carries
// the recovery proof, which never becomes a session.
export class AccountRecoveryRequired extends Error {
  recovery?: unknown

  constructor(recovery: unknown) {
    super("This account is scheduled for deletion.")
    this.name = "AccountRecoveryRequired"
    this.recovery = recovery
  }
}

// Move the proof out of the error before React Query retains that error.
// It belongs only to the current confirmation screen, never session storage.
export function takeAccountRecovery(error: unknown): RecoveryState | undefined {
  if (!(error instanceof AccountRecoveryRequired)) return
  const value = error.recovery
  delete error.recovery
  return readRecovery(value)
}

export async function confirmAccountRecovery(token: string): Promise<void> {
  await authApi<void>("/account/recovery/confirm", {
    method: "POST",
    body: { token },
  })
}
