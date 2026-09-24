import { describe, expect, it } from "vitest"

import {
  createTranslator,
  resolveMessages,
  type BillingUiMessageBundle,
} from "../i18n/messages"
import { de } from "../locales/de"
import { es } from "../locales/es"
import { ja } from "../locales/ja"
import { ko } from "../locales/ko"
import { zh } from "../locales/zh"
import {
  accessLabel,
  everyLabel,
  intervalHours,
  perLabel,
  periodOf,
} from "./period"

const translator = (locale: string, bundle?: BillingUiMessageBundle) =>
  createTranslator(resolveMessages(bundle), undefined, locale)

describe("periodOf", () => {
  it.each([
    [1, "hour", 1],
    [36, "hour", 36],
    [24, "day", 1],
    [720, "day", 30],
    [744, "day", 31],
    [8760, "day", 365],
    [168, "week", 1],
    [336, "week", 2],
  ])("%ih is %s × %i", (hours, unit, count) => {
    expect(periodOf(hours)).toEqual({ unit, count })
  })

  it.each([0, -24, 1.5, null, undefined, Number.NaN])("rejects %s", (h) => {
    expect(periodOf(h)).toBeNull()
  })

  it("parses OpenRails intervals", () => {
    expect(intervalHours("720h")).toBe(720)
    expect(intervalHours("720h0m0s")).toBe(720)
    expect(intervalHours("30d")).toBeNull()
    expect(intervalHours(null)).toBeNull()
  })
})

// 720h is 30 days in OpenRails, never "a month"; 8760h is never "a year".
const hours = [1, 2, 24, 48, 168, 336, 720, 8760, 1000] as const
const every: Record<string, string[]> = {
  en: [
    "every hour",
    "every 2 hours",
    "every day",
    "every 2 days",
    "every week",
    "every 2 weeks",
    "every 30 days",
    "every 365 days",
    "every 1,000 hours",
  ],
  de: [
    "stündlich",
    "alle 2 Stunden",
    "täglich",
    "alle 2 Tage",
    "wöchentlich",
    "alle 2 Wochen",
    "alle 30 Tage",
    "alle 365 Tage",
    "alle 1.000 Stunden",
  ],
  es: [
    "cada hora",
    "cada 2 horas",
    "cada día",
    "cada 2 días",
    "cada semana",
    "cada 2 semanas",
    "cada 30 días",
    "cada 365 días",
    "cada 1000 horas",
  ],
  ja: [
    "毎時",
    "2時間ごと",
    "毎日",
    "2日ごと",
    "毎週",
    "2週間ごと",
    "30日ごと",
    "365日ごと",
    "1,000時間ごと",
  ],
  ko: [
    "매시간",
    "2시간마다",
    "매일",
    "2일마다",
    "매주",
    "2주마다",
    "30일마다",
    "365일마다",
    "1,000시간마다",
  ],
  zh: [
    "每小时",
    "每 2 小时",
    "每天",
    "每 2 天",
    "每周",
    "每 2 周",
    "每 30 天",
    "每 365 天",
    "每 1,000 小时",
  ],
}
const bundles: Record<string, BillingUiMessageBundle | undefined> = {
  en: undefined,
  de,
  es,
  ja,
  ko,
  zh,
}

describe.each(Object.keys(every))("%s labels", (locale) => {
  const m = translator(locale, bundles[locale])

  it("names each renewal period exactly", () => {
    expect(hours.map((h) => everyLabel(h, m))).toEqual(every[locale])
  })

  it("prices per period and non-renewing access", () => {
    const one = {
      en: "/ day",
      de: "/ Tag",
      es: "/ día",
      ja: "/日",
      ko: "/일",
      zh: "/ 天",
    }
    const thirty = {
      en: "/ 30 days",
      de: "/ 30 Tage",
      es: "/ 30 días",
      ja: "/30日",
      ko: "/30일",
      zh: "/ 30 天",
    }
    const access = {
      en: ["1 day of access", "30 days of access"],
      de: ["1 Tag Zugang", "30 Tage Zugang"],
      es: ["1 día de acceso", "30 días de acceso"],
      ja: ["1日間のアクセス", "30日間のアクセス"],
      ko: ["1일 이용", "30일 이용"],
      zh: ["1 天访问权限", "30 天访问权限"],
    }
    const key = locale as keyof typeof one
    expect(perLabel(24, m)).toBe(one[key])
    expect(perLabel(720, m)).toBe(thirty[key])
    expect([accessLabel(24, m), accessLabel(720, m)]).toEqual(access[key])
  })
})

describe("plural lookup", () => {
  it("uses the host hook per plural form", () => {
    const m = createTranslator(
      resolveMessages(),
      (key, vars) =>
        key === "interval.every.day.other" ? `each ${vars?.count}d` : undefined,
      "en"
    )
    expect(everyLabel(720, m)).toBe("each 30d")
    expect(everyLabel(24, m)).toBe("every day")
  })

  it("falls back to other for a category the bundle omits", () => {
    const m = translator("es", es)
    // es selects "many" for a million; the bundle only spells one/other.
    expect(m.plural("interval.every.hour", 1_000_000)).toBe(
      "cada 1.000.000 horas"
    )
  })
})
