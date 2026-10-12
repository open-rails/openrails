import { act, render } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  PAYMENT_COMPLETE_MESSAGE,
  PAYMENT_RESIZE_MESSAGE,
  EmbeddedPayment,
} from "./embedded-payment"

const url = "https://purchase.example/pay/ord_1#pus_secret"

function post(
  frame: HTMLIFrameElement,
  data: unknown,
  origin = "https://purchase.example",
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
    post(frame, complete, "https://purchase.example", window)
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
})
