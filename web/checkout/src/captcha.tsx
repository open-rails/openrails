// A pay the server answers 403 captcha_required (card testing) is resent once
// with the token the customer earns in the deployment's captcha widget.
import { useEffect, useRef, useSyncExternalStore } from "react"
import type { CaptchaConfig } from "@openrails/billing-ui/client"

import type { CheckoutFetch } from "./api"

interface Pending {
  captcha: CaptchaConfig
  resolve: (token: string) => void
  reject: (cause: Error) => void
}

type Widget = { render(el: HTMLElement, options: Record<string, unknown>): unknown }

declare global {
  interface Window {
    turnstile?: Widget
    hcaptcha?: Widget
    grecaptcha?: { ready(fn: () => void): void; execute(key: string, options: { action: string }): Promise<string> }
  }
}

let pending: Pending | null = null
const listeners = new Set<() => void>()
const set = (next: Pending | null) => {
  pending = next
  listeners.forEach((l) => l())
}

function solve(captcha: CaptchaConfig): Promise<string> {
  return new Promise((resolve, reject) => {
    set({
      captcha,
      resolve: (token) => (set(null), resolve(token)),
      reject: (cause) => (set(null), reject(cause)),
    })
  })
}

export function captchaFetch(inner: CheckoutFetch, captcha: () => CaptchaConfig | null | undefined): CheckoutFetch {
  return async (input, init = {}) => {
    const res = await inner(input, init)
    const config = captcha()
    if (res.status !== 403 || !config) return res
    const body = (await res.clone().json().catch(() => null)) as { error?: { code?: string } } | null
    if (body?.error?.code !== "captcha_required") return res
    const token = await solve(config)
    const headers = new Headers(init.headers)
    headers.set(config.token_header, token)
    return inner(input, { ...init, headers })
  }
}

const loaded = new Map<string, Promise<void>>()
function load(src: string): Promise<void> {
  let script = loaded.get(src)
  if (!script) {
    script = new Promise((resolve, reject) => {
      const el = document.createElement("script")
      el.src = src
      el.async = true
      el.onload = () => resolve()
      el.onerror = () => reject(new Error("The challenge could not load."))
      document.head.appendChild(el)
    })
    loaded.set(src, script)
  }
  return script
}

/** The challenge, while a pay waits for one. */
export function CaptchaPrompt() {
  const current = useSyncExternalStore(
    (l) => (listeners.add(l), () => listeners.delete(l)),
    () => pending
  )
  const host = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!current || !host.current) return
    const { captcha, resolve, reject } = current
    const el = host.current
    load(captcha.script_url)
      .then(() => {
      if (captcha.provider === "recaptcha-v3" && window.grecaptcha) {
        const g = window.grecaptcha
        g.ready(() => g.execute(captcha.site_key, { action: captcha.action }).then(resolve, reject))
        return
      }
      const widget = captcha.provider === "hcaptcha" ? window.hcaptcha : window.turnstile
      if (!widget) throw new Error("The challenge could not load.")
      widget.render(el, {
        sitekey: captcha.site_key,
        callback: (token: string) => resolve(token),
        "error-callback": () => reject(new Error("The challenge failed.")),
      })
    })
      .catch(reject)
  }, [current])
  if (!current) return null
  return (
    <section className="captcha" aria-label="Verification">
      <p className="muted">Confirm you are not a robot to continue.</p>
      <div ref={host} />
    </section>
  )
}
