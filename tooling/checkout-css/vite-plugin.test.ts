import { describe, expect, it } from "vitest"

import { renderStyleInstaller } from "./vite-plugin"

describe("checkout CSS Vite plugin", () => {
  it("installs one replaceable browser style element", () => {
    const installer = renderStyleInstaller(".orck{display:block}")

    expect(installer).toContain('typeof document !== "undefined"')
    expect(installer).toContain("openrails-checkout-styles")
    expect(installer).toContain("document.getElementById")
  })
})
