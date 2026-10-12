import { describe, expect, it } from "vitest"

import { isolateCss } from "./isolate"

describe("purchase CSS isolation", () => {
  it("removes layers and scopes utilities to payment page surfaces", async () => {
    const css = await isolateCss(`
      @layer theme, utilities;
      @layer theme { :root, :host { --spacing: .25rem; } }
      @layer utilities {
        .hidden { display: none; }
        .orck { color: black; }
        .orck-collect-field iframe { display: block; }
      }
    `)

    expect(css).not.toContain("@layer")
    expect(css).toContain(
      '.orck, :where([data-slot="dialog-portal"]:has(> .orck[data-slot="dialog-content"]), [data-slot="alert-dialog-portal"]:has(> .orck[data-slot="alert-dialog-content"])) { --spacing: .25rem; }'
    )
    expect(css).toContain(".orck .hidden")
    expect(css).toContain(".orck.hidden")
    expect(css).toContain(
      ':where([data-slot="dialog-portal"]:has(> .orck[data-slot="dialog-content"]), [data-slot="alert-dialog-portal"]:has(> .orck[data-slot="alert-dialog-content"])) .hidden'
    )
    expect(css).toContain(".orck-collect-field iframe")
    expect(css).not.toMatch(/(^|[},])\s*\.hidden\s*\{/)
  })

  it("namespaces Tailwind variables and animation names", async () => {
    const css = await isolateCss(`
      @property --tw-duration { syntax: "*"; inherits: false; }
      @keyframes spin { to { transform: rotate(360deg); } }
      .animate-spin { --tw-duration: 1s; animation: spin var(--tw-duration); }
    `)

    expect(css).toContain("@property --orck-tw-duration")
    expect(css).toContain("@keyframes orck-spin")
    expect(css).toContain("--orck-tw-duration")
    expect(css).toContain("animation: orck-spin var(--orck-tw-duration)")
  })
})
