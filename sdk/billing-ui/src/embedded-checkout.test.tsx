import { act, render } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  CHECKOUT_COMPLETE_MESSAGE,
  CHECKOUT_RESIZE_MESSAGE,
  EmbeddedCheckout,
} from "./embedded-checkout"

const url = "https://checkout.example/c/ord_1#cks_secret"

function post(
  frame: HTMLIFrameElement,
  data: unknown,
  origin = "https://checkout.example",
  source: MessageEventSource | null = frame.contentWindow
) {
  act(() => {
    window.dispatchEvent(new MessageEvent("message", { data, origin, source }))
  })
}

describe("EmbeddedCheckout", () => {
  it("frames the checkout URL with payment and passkeys allowed", () => {
    const { container } = render(<EmbeddedCheckout url={url} />)
    const frame = container.querySelector("iframe")!
    expect(frame.getAttribute("src")).toBe(url)
    expect(frame.getAttribute("allow")).toContain("payment")
    expect(frame.getAttribute("allow")).toContain("publickey-credentials-get")
  })

  it("hears completion and height only from its own frame on the checkout origin", () => {
    const onComplete = vi.fn()
    const { container } = render(
      <EmbeddedCheckout url={url} onComplete={onComplete} />
    )
    const frame = container.querySelector("iframe")!
    const complete = {
      type: CHECKOUT_COMPLETE_MESSAGE,
      order_id: "ord_1",
      success_url: "https://shop.example/thanks",
    }
    post(frame, complete, "https://evil.example")
    post(frame, complete, "https://checkout.example", window)
    expect(onComplete).not.toHaveBeenCalled()

    post(frame, { type: CHECKOUT_RESIZE_MESSAGE, height: 812.4 })
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
    const { container } = render(<EmbeddedCheckout url="javascript:alert(1)" />)
    expect(container.querySelector("iframe")).toBeNull()
  })
})
