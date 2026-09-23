import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { Button } from "#orck/components/ui/button"

describe("ui class merging", () => {
  it("lets className win Tailwind conflicts over variant classes", () => {
    render(<Button className="h-10 bg-secondary px-2">Go</Button>)
    const cls = screen.getByRole("button", { name: "Go" }).className.split(" ")
    expect(cls).toEqual(
      expect.arrayContaining(["h-10", "bg-secondary", "px-2"])
    )
    expect(cls).toContain("hover:bg-primary/80")
    expect(cls).not.toContain("h-9")
    expect(cls).not.toContain("px-2.5")
    expect(cls).not.toContain("bg-primary")
  })
})
