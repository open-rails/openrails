import type { UpsertProviderRequest } from "@/lib/api/endpoints"

// Keep only a digest and operation metadata while an outcome is uncertain.
// Re-entering the same write-only credentials retries the original operation,
// even after dismissing the dialog or refreshing the displayed account.
export class ProviderPublicationAttempts {
  private attempts = new Map<
    string,
    { operation_id: string; expected_revision: number }
  >()

  async prepare(
    merchant: string,
    rail: string,
    revision: number,
    body: Omit<UpsertProviderRequest, "operation_id" | "expected_revision">
  ): Promise<UpsertProviderRequest> {
    const ordered = (values?: Record<string, string>) =>
      Object.entries(values ?? {}).sort(([a], [b]) => a.localeCompare(b))
    const encoded = new TextEncoder().encode(
      JSON.stringify([
        merchant,
        rail,
        body.account_id,
        ordered(body.credentials),
        ordered(body.public_config),
      ])
    )
    const digest = Array.from(
      new Uint8Array(await crypto.subtle.digest("SHA-256", encoded)),
      (byte) => byte.toString(16).padStart(2, "0")
    ).join("")
    let attempt = this.attempts.get(digest)
    if (!attempt) {
      attempt = {
        operation_id: crypto.randomUUID(),
        expected_revision: revision,
      }
      this.attempts.set(digest, attempt)
    }
    return { ...body, ...attempt }
  }

  complete(operationID: string) {
    for (const [digest, attempt] of this.attempts) {
      if (attempt.operation_id === operationID) this.attempts.delete(digest)
    }
  }
}
