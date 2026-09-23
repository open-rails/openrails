import { en } from "../locales/en.ts"

/**
 * A count-dependent message: CLDR plural categories for the locale, with
 * `other` required. An exact-count key (`"1"`) wins over the category, like
 * ICU's `=1`, for locales whose single form differs from `{count}` (毎日).
 */
export type PluralMessage = {
  [K in Intl.LDMLPluralRule | "1"]?: string
} & { other: string }

type Widen<T> = {
  [K in keyof T]: T[K] extends string
    ? string
    : T[K] extends PluralMessage
      ? PluralMessage
      : Widen<T[K]>
}

/** The complete message tree; English is the reference shape. */
export type BillingUiMessages = Widen<typeof en>

type DeepPartial<T> = {
  [K in keyof T]?: T[K] extends string
    ? string
    : T[K] extends PluralMessage
      ? Partial<PluralMessage>
      : DeepPartial<T[K]>
}

/** A locale bundle or host override: any subset of the tree. */
export type BillingUiMessageBundle = DeepPartial<BillingUiMessages>

type Paths<T, P extends string = ""> = {
  [K in keyof T & string]: T[K] extends string
    ? `${P}${K}`
    : T[K] extends PluralMessage
      ? never
      : Paths<T[K], `${P}${K}.`>
}[keyof T & string]

type PluralPaths<T, P extends string = ""> = {
  [K in keyof T & string]: T[K] extends string
    ? never
    : T[K] extends PluralMessage
      ? `${P}${K}`
      : PluralPaths<T[K], `${P}${K}.`>
}[keyof T & string]

/** Dotted key of any message, e.g. `subscriptions.title`. */
export type MessageKey = Paths<BillingUiMessages>

/** Dotted key of a count-dependent message, e.g. `interval.every.day`. */
export type PluralKey = PluralPaths<BillingUiMessages>

export type MessageVars = Record<string, string | number>

/**
 * Host translation hook. Receives the bare key (`subscriptions.title`,
 * `errors.not_found`); return nothing or the key itself to use ours.
 */
export type BillingUiTranslate = (
  key: string,
  vars?: MessageVars
) => string | null | undefined

/** Types a locale bundle or override against the message tree. */
export function defineMessages(
  bundle: BillingUiMessageBundle
): BillingUiMessageBundle {
  return bundle
}

export const defaultMessages: BillingUiMessages = en

type Tree = { [key: string]: string | Tree }

function mergeInto(target: Tree, source: Tree): Tree {
  const out: Tree = { ...target }
  for (const [key, value] of Object.entries(source)) {
    if (value === undefined || value === null) continue
    const current = out[key]
    if (typeof value === "string") {
      if (value !== "") out[key] = value
    } else if (typeof current === "object") {
      out[key] = mergeInto(current, value)
    } else {
      out[key] = mergeInto({}, value)
    }
  }
  return out
}

/** Layers bundles over English; later bundles win, missing keys fall back. */
export function resolveMessages(
  bundles?: BillingUiMessageBundle | readonly BillingUiMessageBundle[]
): BillingUiMessages {
  const list = bundles ? (Array.isArray(bundles) ? bundles : [bundles]) : []
  let tree = en as unknown as Tree
  for (const bundle of list) tree = mergeInto(tree, bundle as Tree)
  return tree as unknown as BillingUiMessages
}

export function interpolate(template: string, vars?: MessageVars): string {
  if (!vars) return template
  return template.replace(/\{(\w+)\}/g, (match, name: string) =>
    name in vars ? String(vars[name]) : match
  )
}

function lookup(messages: BillingUiMessages, key: string): string | undefined {
  let node: unknown = messages
  for (const part of key.split(".")) {
    if (!node || typeof node !== "object") return undefined
    node = (node as Tree)[part]
  }
  return typeof node === "string" ? node : undefined
}

export interface Translator {
  messages: BillingUiMessages
  /** BCP 47 tag used for plural rules and numbers; the runtime's when unset. */
  locale?: string
  t(key: MessageKey, vars?: MessageVars): string
  /**
   * Count-dependent message: tries `<key>.<count>`, then the locale's plural
   * category, then `<key>.other`. `{count}` is locale-formatted.
   */
  plural(key: PluralKey, count: number, vars?: MessageVars): string
  /** Message for an billing error, error code, or thrown value. */
  error(error: unknown, vars?: MessageVars): string
}

function errorCode(error: unknown): string | undefined {
  if (typeof error === "string") return error
  if (error && typeof error === "object" && "code" in error) {
    const code = (error as { code: unknown }).code
    if (typeof code === "string") return code
  }
  return undefined
}

function isNetworkError(error: unknown): boolean {
  return error instanceof TypeError && /fetch|network/i.test(error.message)
}

function pluralRules(locale?: string): Intl.PluralRules {
  try {
    return new Intl.PluralRules(locale)
  } catch {
    return new Intl.PluralRules("en")
  }
}

function formatCount(count: number, locale?: string): string {
  try {
    return new Intl.NumberFormat(locale).format(count)
  } catch {
    return String(count)
  }
}

export function createTranslator(
  messages: BillingUiMessages,
  hostT?: BillingUiTranslate,
  locale?: string
): Translator {
  const rules = pluralRules(locale)
  const translate = (key: string, vars?: MessageVars): string | undefined => {
    const hosted = hostT?.(key, vars)
    if (hosted && hosted !== key) return hosted
    const own = lookup(messages, key)
    return own === undefined ? undefined : interpolate(own, vars)
  }
  return {
    messages,
    locale,
    t: (key, vars) => translate(key, vars) ?? key,
    plural(key, count, vars) {
      const all = { ...vars, count: formatCount(count, locale) }
      return (
        translate(`${key}.${count}`, all) ??
        translate(`${key}.${rules.select(count)}`, all) ??
        translate(`${key}.other`, all) ??
        key
      )
    },
    error(error, vars) {
      const code = errorCode(error)
      if (code && code !== "generic" && code !== "network") {
        const metadata =
          error && typeof error === "object" && "metadata" in error
            ? (error as { metadata?: MessageVars }).metadata
            : undefined
        const message = translate(`errors.${code}`, { ...metadata, ...vars })
        if (message) return message
      }
      const fallback = isNetworkError(error)
        ? "errors.network"
        : "errors.generic"
      return translate(fallback, vars) ?? messages.errors.generic
    },
  }
}
