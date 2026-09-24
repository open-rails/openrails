// Collect.js (NMI browser tokenization). The gateway script mounts hosted
// iframes into our field containers, so card data never touches this code —
// we receive a one-time token plus display metadata. Collect.js has no
// teardown API: the script and configuration persist for the page lifetime;
// reconfiguring re-mounts fields.
import * as React from "react"

export interface CollectCard {
  number?: string
  type?: string
  exp?: string
}

export interface CollectResponse {
  token: string
  card?: CollectCard
}

declare global {
  interface Window {
    CollectJS?: {
      configure: (config: Record<string, unknown>) => void
      startPaymentRequest: () => void
    }
  }
}

const SCRIPT_ID = "openrails-collectjs"
const TIMEOUT_MS = 30_000
const collectScriptStates = new WeakMap<
  HTMLScriptElement,
  "loading" | "loaded" | "failed"
>()

function safeCollectScriptURL(raw: string): string | undefined {
  try {
    const parsed = new URL(raw)
    if (
      parsed.protocol !== "https:" ||
      parsed.username ||
      parsed.password ||
      parsed.hash
    ) {
      return undefined
    }
    return parsed.toString()
  } catch {
    return undefined
  }
}

// toPlainColor normalizes a CSS color into rgb()/rgba() form, the least
// common denominator the gateway's CSS sanitizer accepts. Chrome serializes
// the theme tokens in their authored oklch space (canvas fillStyle keeps it
// too), so oklch is converted numerically; everything else round-trips
// through a canvas fillStyle. Unparseable input returns "".
export function toPlainColor(value: string | undefined): string {
  if (!value) return ""
  const oklch = oklchToRGB(value)
  if (oklch) return oklch
  try {
    const context = document.createElement("canvas").getContext("2d")
    if (!context) return ""
    context.fillStyle = "#010203"
    context.fillStyle = value
    const normalized = context.fillStyle
    if (normalized === "#010203" && value !== "#010203") return ""
    return normalized.startsWith("oklch") ? "" : normalized
  } catch {
    return ""
  }
}

// oklchToRGB converts "oklch(L C H)" / "oklch(L C H / A)" (L as 0..1 or %)
// to an rgb()/rgba() string via OKLab → linear sRGB (Ottosson's matrices).
function oklchToRGB(value: string): string {
  const match =
    /^oklch\(\s*([\d.]+%?)\s+([\d.]+)\s+([\d.]+)(?:deg)?\s*(?:\/\s*([\d.]+%?)\s*)?\)$/i.exec(
      value.trim()
    )
  if (!match) return ""
  const number = (raw: string) =>
    raw.endsWith("%") ? parseFloat(raw) / 100 : parseFloat(raw)
  const L = number(match[1])
  const C = parseFloat(match[2])
  const H = (parseFloat(match[3]) * Math.PI) / 180
  const alpha = match[4] === undefined ? 1 : number(match[4])
  const a = C * Math.cos(H)
  const b = C * Math.sin(H)
  const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3
  const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3
  const s = (L - 0.0894841775 * a - 1.291485548 * b) ** 3
  const channel = (linear: number) => {
    const gamma =
      linear <= 0.0031308
        ? 12.92 * linear
        : 1.055 * Math.max(linear, 0) ** (1 / 2.4) - 0.055
    return Math.round(Math.min(1, Math.max(0, gamma)) * 255)
  }
  const red = channel(4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s)
  const green = channel(-1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s)
  const blue = channel(-0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s)
  return alpha >= 1
    ? `rgb(${red}, ${green}, ${blue})`
    : `rgba(${red}, ${green}, ${blue}, ${alpha})`
}

// Preview keys skip the live script entirely so fixture-driven previews can
// render the card fields without a real gateway.
export function isPreviewTokenizationKey(key: string): boolean {
  return key.startsWith("preview_")
}

/** Inline field errors, keyed by our field names. */
export interface CollectFieldErrors {
  number?: string
  expiry?: string
  cvv?: string
}

const FIELD_NAMES: Record<string, keyof CollectFieldErrors> = {
  ccnumber: "number",
  ccexp: "expiry",
  cvv: "cvv",
}

const FIELD_MESSAGES: Record<keyof CollectFieldErrors, string> = {
  number: "Enter a valid card number",
  expiry: "Enter a valid expiry date",
  cvv: "Enter a valid security code",
}

export interface CollectFieldSelectors {
  number: string
  expiry: string
  cvv: string
}

export function useCollectJS(config: {
  enabled: boolean
  active: boolean
  tokenizationKey: string
  scriptURL: string
  selectors: CollectFieldSelectors
}): {
  ready: boolean
  preview: boolean
  loadError?: string
  /** Every field reported valid by the gateway. */
  valid: boolean
  fieldErrors: CollectFieldErrors
  tokenize: () => Promise<CollectResponse>
} {
  const preview =
    config.enabled && isPreviewTokenizationKey(config.tokenizationKey)
  const [ready, setReady] = React.useState(false)
  const [loadError, setLoadError] = React.useState<string>()
  // Validity per field as Collect.js reports it on edit and blur.
  const [validity, setValidity] = React.useState<
    Partial<
      Record<keyof CollectFieldErrors, { valid: boolean; message?: string }>
    >
  >({})
  const pending = React.useRef<
    | {
        resolve: (r: CollectResponse) => void
        reject: (e: Error) => void
        timer: number
      }
    | undefined
  >(undefined)
  const configuredFor = React.useRef<string | undefined>(undefined)
  const { number, expiry, cvv } = config.selectors

  React.useEffect(() => {
    if (
      !config.enabled ||
      preview ||
      !config.tokenizationKey ||
      !config.scriptURL ||
      typeof document === "undefined"
    ) {
      return
    }
    const scriptURL = safeCollectScriptURL(config.scriptURL)
    if (!scriptURL) {
      void Promise.resolve().then(() => {
        setReady(false)
        setLoadError("Card fields failed to load")
      })
      return
    }
    const configuration = [
      config.tokenizationKey,
      scriptURL,
      number,
      expiry,
      cvv,
    ].join("\u0000")
    const configurationChanged = configuredFor.current !== configuration
    if (!configurationChanged && !config.active) return
    configuredFor.current = configuration

    let cancelled = false
    const settle = (fn: () => void) => {
      if (!cancelled) fn()
    }
    const configure = () => {
      if (cancelled) return
      const collect = window.CollectJS
      if (!collect) {
        settle(() => setLoadError("Card fields failed to load"))
        return
      }
      settle(() => {
        // Replacing stale hosted iframes invalidates readiness until NMI
        // confirms that every new field is available.
        setReady(false)
        setLoadError(undefined)
        setValidity({})
      })
      // The hosted iframes cannot inherit the page theme; hand them the
      // container's resolved colors so dark mode reaches inside the fields.
      // Chrome serializes computed colors in their authored space (oklch),
      // which the gateway's CSS sanitizer drops — normalize through a canvas
      // fillStyle round-trip to a format it accepts. (Resolved once per
      // configure — an in-place OS theme flip re-themes on the next load.)
      const probe = document.querySelector<HTMLElement>(number)
      const probeStyle = probe ? window.getComputedStyle(probe) : undefined
      const probeBackground = toPlainColor(probeStyle?.backgroundColor)
      const fieldBackground =
        probeBackground &&
        probeBackground !== "transparent" &&
        probeBackground !== "rgba(0, 0, 0, 0)"
          ? probeBackground
          : "#ffffff"
      const fieldColor = toPlainColor(probeStyle?.color) || "#18181b"
      const placeholderColor =
        toPlainColor(
          probeStyle?.getPropertyValue("--muted-foreground").trim()
        ) || fieldColor
      try {
        collect.configure({
          variant: "inline",
          styleSniffer: true,
          customCss: {
            height: "38px",
            "line-height": "38px",
            padding: "0 8px",
            "font-size": "14px",
            "font-family": probeStyle?.fontFamily || "inherit",
            "background-color": fieldBackground,
            color: fieldColor,
          },
          placeholderCss: {
            color: placeholderColor,
          },
          invalidCss: {
            color:
              toPlainColor(
                probeStyle?.getPropertyValue("--destructive").trim()
              ) || "#dc2626",
          },
          focusCss: {
            outline: "none",
          },
          // Collect.js owns the cross-origin inputs and therefore controls
          // their autocomplete behavior. Titles and placeholders are the
          // accessibility/browser hints its public field API supports.
          fields: {
            ccnumber: {
              selector: number,
              title: "Card number",
              placeholder: "1234 1234 1234 1234",
            },
            ccexp: {
              selector: expiry,
              title: "Expiration date",
              placeholder: "MM / YY",
            },
            cvv: {
              selector: cvv,
              title: "Card security code",
              placeholder: "CVC",
            },
          },
          fieldsAvailableCallback: () => settle(() => setReady(true)),
          validationCallback: (
            field: string,
            status: boolean,
            message: string
          ) => {
            const name = FIELD_NAMES[field]
            if (!name) return
            settle(() =>
              setValidity((current) => ({
                ...current,
                [name]: {
                  valid: status,
                  message: status
                    ? undefined
                    : message && message !== "Field is empty"
                      ? `${FIELD_MESSAGES[name]}.`
                      : FIELD_MESSAGES[name],
                },
              }))
            )
          },
          timeoutDuration: TIMEOUT_MS,
          timeoutCallback: () => {
            const request = pending.current
            pending.current = undefined
            if (request) {
              window.clearTimeout(request.timer)
              request.reject(new Error("Card entry timed out"))
            }
          },
          callback: (response: CollectResponse) => {
            const request = pending.current
            pending.current = undefined
            if (request) {
              window.clearTimeout(request.timer)
              request.resolve(response)
            }
          },
        })
      } catch {
        settle(() => setLoadError("Card fields failed to load"))
      }
    }
    let existing = document.getElementById(SCRIPT_ID)
    if (
      existing instanceof HTMLScriptElement &&
      (existing.src !== scriptURL ||
        collectScriptStates.get(existing) === "failed")
    ) {
      // A white-label merchant may use a different Collect.js distribution.
      // Never configure one merchant's key against another merchant's script,
      // and allow a later modal open to retry a failed network load.
      existing.remove()
      if (existing.src !== scriptURL) {
        Reflect.deleteProperty(window, "CollectJS")
      }
      existing = null
    }
    if (existing instanceof HTMLScriptElement) {
      if (window.CollectJS) {
        configure()
        return () => {
          cancelled = true
        }
      }
      if (collectScriptStates.get(existing) === "loaded") {
        settle(() => setLoadError("Card fields failed to load"))
        return () => {
          cancelled = true
        }
      }
      const failed = () =>
        settle(() => setLoadError("Card fields failed to load"))
      existing.addEventListener("load", configure)
      existing.addEventListener("error", failed)
      return () => {
        cancelled = true
        existing.removeEventListener("load", configure)
        existing.removeEventListener("error", failed)
      }
    }
    const script = document.createElement("script")
    script.id = SCRIPT_ID
    script.src = scriptURL
    script.async = true
    script.dataset.tokenizationKey = config.tokenizationKey
    collectScriptStates.set(script, "loading")
    script.onload = () => {
      collectScriptStates.set(script, "loaded")
      configure()
    }
    script.onerror = () => {
      collectScriptStates.set(script, "failed")
      settle(() => setLoadError("Card fields failed to load"))
    }
    document.head.appendChild(script)
    return () => {
      cancelled = true
    }
  }, [
    config.enabled,
    config.active,
    preview,
    config.tokenizationKey,
    config.scriptURL,
    number,
    expiry,
    cvv,
  ])

  const tokenize = React.useCallback(
    () =>
      new Promise<CollectResponse>((resolve, reject) => {
        if (preview) {
          // Preview sessions demo the full flow; a real backend rejects this
          // token, fixture sources accept it.
          resolve({
            token: "preview_payment_token",
            card: { number: "4242424242424242", type: "visa", exp: "1227" },
          })
          return
        }
        const collect = window.CollectJS
        if (!collect || !ready) {
          reject(new Error("Card fields are not ready"))
          return
        }
        if (pending.current) {
          reject(new Error("Card entry is already in progress"))
          return
        }
        const timer = window.setTimeout(() => {
          pending.current = undefined
          reject(new Error("Card entry timed out"))
        }, TIMEOUT_MS + 5_000)
        pending.current = { resolve, reject, timer }
        collect.startPaymentRequest()
      }),
    [preview, ready]
  )

  const fieldErrors: CollectFieldErrors = {}
  for (const name of ["number", "expiry", "cvv"] as const) {
    const state = validity[name]
    if (state && !state.valid) fieldErrors[name] = state.message
  }
  const valid =
    preview ||
    (["number", "expiry", "cvv"] as const).every(
      (name) => validity[name]?.valid === true
    )
  return {
    ready: ready || preview,
    preview,
    loadError,
    valid,
    fieldErrors,
    tokenize,
  }
}

/** Display metadata of a tokenized card: never the PAN. */
export function collectCardDisplay(card?: CollectCard): {
  last_four?: string
  card_type?: string
  expiry_date?: string
} {
  const digits = (card?.number ?? "").replace(/\D/g, "")
  const exp = (card?.exp ?? "").replace(/\D/g, "")
  return {
    ...(digits.length >= 4 ? { last_four: digits.slice(-4) } : {}),
    ...(card?.type ? { card_type: card.type } : {}),
    ...(exp.length === 4
      ? { expiry_date: `${exp.slice(0, 2)}/${exp.slice(2)}` }
      : {}),
  }
}
