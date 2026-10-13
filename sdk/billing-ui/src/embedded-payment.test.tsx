import { act, render } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  PAYMENT_COMPLETE_MESSAGE,
  PAYMENT_RESIZE_MESSAGE,
  EmbeddedPayment,
} from "./embedded-payment"
import { readPaymentFragment, withPaymentFallbacks } from "./fallbacks"

const url = "https://pay.example/pay/ord_1#pus_secret"

function post(
  frame: HTMLIFrameElement,
  data: unknown,
  origin = "https://pay.example",
  source: MessageEventSource | null = frame.contentWindow
) {
  act(() => {
    window.dispatchEvent(new MessageEvent("message", { data, origin, source }))
  })
}

describe("EmbeddedPayment", () => {
  it("frames the pay URL with payment and passkeys allowed", () => {
    const { container } = render(<EmbeddedPayment url={url} />)
    const frame = container.querySelector("iframe")!
    expect(frame.getAttribute("src")).toBe(url)
    expect(frame.getAttribute("allow")).toContain("payment")
    expect(frame.getAttribute("allow")).toContain("publickey-credentials-get")
  })

  it("hears completion and height only from its own frame on the payment origin", () => {
    const onComplete = vi.fn()
    const { container } = render(
      <EmbeddedPayment url={url} onComplete={onComplete} />
    )
    const frame = container.querySelector("iframe")!
    const complete = {
      type: PAYMENT_COMPLETE_MESSAGE,
      order_id: "ord_1",
      success_url: "https://shop.example/thanks",
    }
    post(frame, complete, "https://evil.example")
    post(frame, complete, "https://pay.example", window)
    expect(onComplete).not.toHaveBeenCalled()

    post(frame, { type: PAYMENT_RESIZE_MESSAGE, height: 812.4 })
    expect(frame.style.height).toBe("813px")

    post(frame, complete)
    post(frame, complete)
    expect(onComplete).toHaveBeenCalledTimes(1)
    expect(onComplete).toHaveBeenCalledWith({
      orderId: "ord_1",
      successUrl: "https://shop.example/thanks",
    })
  })

  it("renders nothing for a URL that is not http(s)", () => {
    const { container } = render(<EmbeddedPayment url="javascript:alert(1)" />)
    expect(container.querySelector("iframe")).toBeNull()
  })

  it("hands its fallbacks to the framed page in the fragment, after the secret", () => {
    const fallbacks = { ccbill: { for: ["mobius"], afterFailures: 2 } }
    const { container } = render(
      <EmbeddedPayment url={url} fallbacks={fallbacks} />
    )
    const src = container.querySelector("iframe")!.getAttribute("src")!
    expect(src.startsWith(`${url}&fallbacks=`)).toBe(true)
    expect(readPaymentFragment(new URL(src).hash)).toEqual({
      secret: "pus_secret",
      fallbacks,
    })
    expect(readPaymentFragment("#pus_secret")).toEqual({ secret: "pus_secret" })
  })
})

describe("readPaymentFragment", () => {
  const read = (value: unknown) =>
    readPaymentFragment(
      `#pus_s&fallbacks=${encodeURIComponent(typeof value === "string" ? value : JSON.stringify(value))}`
    )

  it.each([
    ["not JSON", "{ccbill"],
    ["a list", [{ for: ["mobius"], afterFailures: 2 }]],
    [
      "an unknown field",
      { ccbill: { for: ["mobius"], afterFailures: 2, show: true } },
    ],
    ["no PSP to stand in for", { ccbill: { for: [], afterFailures: 2 } }],
    [
      "a key that is no PSP key",
      { "CC Bill": { for: ["mobius"], afterFailures: 2 } },
    ],
    [
      "a prototype key",
      JSON.parse('{"__proto__": {"for": ["mobius"], "afterFailures": 2}}'),
    ],
    ["no failures", { ccbill: { for: ["mobius"], afterFailures: 0 } }],
    ["part of a failure", { ccbill: { for: ["mobius"], afterFailures: 1.5 } }],
    ["too many failures", { ccbill: { for: ["mobius"], afterFailures: 101 } }],
    [
      "too many fallbacks",
      Object.fromEntries(
        Array.from({ length: 9 }, (_, i) => [
          `psp${i}`,
          { for: ["mobius"], afterFailures: 2 },
        ])
      ),
    ],
    [
      "more than 1024 characters",
      Object.fromEntries(
        ["a", "b", "c"].map((k) => [
          k,
          {
            for: Array.from({ length: 8 }, (_, i) => `${"m".repeat(50)}${i}`),
            afterFailures: 2,
          },
        ])
      ),
    ],
    [
      "a fragment over 4096 characters",
      { ccbill: { for: ["mobius"], afterFailures: 2 }, pad: "x".repeat(4100) },
    ],
  ])("keeps the secret and drops fallbacks with %s", (_, value) => {
    expect(read(value)).toEqual({ secret: "pus_s" })
  })

  it("reads no secret from a fragment that holds none", () => {
    expect(readPaymentFragment("#fallbacks=%7B%7D")).toEqual({ secret: null })
  })

  it("leaves a pay URL without fallbacks as it is", () => {
    expect(withPaymentFallbacks(url)).toBe(url)
    expect(withPaymentFallbacks(url, {})).toBe(url)
  })
})
