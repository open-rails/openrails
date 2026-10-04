import type { CreatePSPRequest, UpdatePSPRequest } from "@/lib/api/endpoints"

// Keep only a digest and operation metadata while an outcome is uncertain.
// Re-entering the same write-only credentials retries the original operation,
// even after dismissing the dialog or refreshing the displayed PSP.
export class PSPPublicationAttempts {
  private attempts = new Map<string, string>()

  // identity names the write: the merchant plus the new PSP's rail and
  // account, or the PSP id and the revision the form read.
  async prepare<T extends Omit<CreatePSPRequest, "operation_id"> | Omit<UpdatePSPRequest, "operation_id">>(
    identity: unknown[],
    body: T
  ): Promise<T & { operation_id: string }> {
    const ordered = (values?: Record<string, string>) =>
      Object.entries(values ?? {}).sort(([a], [b]) => a.localeCompare(b))
    const encoded = new TextEncoder().encode(
      JSON.stringify([
        ...identity,
        "key" in body ? body.key : "",
        "account_id" in body ? body.account_id : "",
        ordered(body.credentials),
        ordered(body.settings),
      ])
    )
    const digest = Array.from(
      new Uint8Array(await crypto.subtle.digest("SHA-256", encoded)),
      (byte) => byte.toString(16).padStart(2, "0")
    ).join("")
    let operation = this.attempts.get(digest)
    if (!operation) {
      operation = crypto.randomUUID()
      this.attempts.set(digest, operation)
    }
    return { ...body, operation_id: operation }
  }

  complete(operationID: string) {
    for (const [digest, operation] of this.attempts) {
      if (operation === operationID) this.attempts.delete(digest)
    }
  }
}
