import { useEffect, useState } from "react"
import type { BillingAdminMerchant } from "@openrails/billing-ui/admin"
import type { MerchantPage } from "../config"
import type { ConsoleClient } from "./session"
import { consoleURL } from "./session"

export function useMerchantDirectory(session: ConsoleClient, subject: string) {
  const [attempt, setAttempt] = useState(0)
  const [result, setResult] = useState<{
    session: ConsoleClient
    subject: string
    attempt: number
    merchants: BillingAdminMerchant[]
    root: boolean
    error?: Error
  }>()
  useEffect(() => {
    const abort = new AbortController()
    async function read() {
      const merchants: BillingAdminMerchant[] = []
      const cursors = new Set<string>()
      let cursor: string | null = null
      let root = false
      do {
        const url = new URL(consoleURL("/merchants"))
        if (cursor) url.searchParams.set("cursor", cursor)
        const response = await session.client.authFetch(url.href, {
          signal: abort.signal,
        })
        if (!response.ok)
          throw new Error(`Merchant directory failed (${response.status}).`)
        const page = (await response.json()) as MerchantPage
        merchants.push(...page.data)
        root = page.root
        cursor = page.next_cursor
        if (cursor && cursors.has(cursor))
          throw new Error("Merchant directory repeated a page.")
        if (cursor) cursors.add(cursor)
      } while (cursor)
      if (!abort.signal.aborted)
        setResult({ session, subject, attempt, merchants, root })
    }
    void read().catch((error: unknown) => {
      if (!abort.signal.aborted)
        setResult({
          session,
          subject,
          attempt,
          merchants: [],
          root: false,
          error: error instanceof Error ? error : new Error(String(error)),
        })
    })
    return () => abort.abort()
  }, [session, subject, attempt])
  const current =
    result?.session === session &&
    result.subject === subject &&
    result.attempt === attempt
      ? result
      : undefined
  return {
    data: current,
    pending: !current,
    retry: () => setAttempt((value) => value + 1),
  }
}
