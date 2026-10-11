// @vitest-environment jsdom
// The console's confirm answers true only on its confirm button, false on
// cancel; a lazy page whose chunk is gone after a deploy offers a reload.
import { createMemoryRouter, RouterProvider } from "react-router-dom"
import { afterEach, beforeEach, expect, it, vi } from "vitest"

import { browserEnvironment, click, mount, unmount } from "@/test/mount"

import { useConfirm } from "@/lib/confirm"
import { isChunkLoadError } from "@/lib/chunk-error"

import { RouteError } from "./route-error"

beforeEach(browserEnvironment)
afterEach(unmount)

const answers: boolean[] = []

function Asker() {
  const confirm = useConfirm()
  return (
    <button
      onClick={async () =>
        answers.push(
          await confirm({
            title: "Archive psp-a?",
            description: "New checkout stops routing to it.",
            confirmLabel: "Archive PSP",
            tone: "destructive",
          })
        )
      }
    >
      Ask
    </button>
  )
}

it("resolves true on confirm and false on cancel", async () => {
  await mount(<Asker />)
  await click("Ask")
  await vi.waitFor(() =>
    expect(document.body.textContent).toContain("Archive psp-a?")
  )
  expect(document.body.textContent).toContain(
    "New checkout stops routing to it."
  )
  await click("Archive PSP")
  await click("Ask")
  await vi.waitFor(() =>
    expect(document.body.textContent).toContain("Archive psp-a?")
  )
  await click("Cancel")
  await vi.waitFor(() => expect(answers).toEqual([true, false]))
})

it("offers a reload when a page's chunk is gone", async () => {
  const router = createMemoryRouter([
    {
      path: "/",
      errorElement: <RouteError />,
      lazy: () =>
        Promise.reject(
          new TypeError(
            "Failed to fetch dynamically imported module: /admin/assets/reports-1a2b.js"
          )
        ),
    },
  ])
  await mount(<RouterProvider router={router} />)
  await vi.waitFor(() =>
    expect(document.body.textContent).toContain(
      "The console has been updated. Reload to use the new version."
    )
  )
  expect(
    [...document.querySelectorAll("button")].map((b) => b.textContent)
  ).toEqual(["Reload"])
  expect(isChunkLoadError(new Error("Unable to preload CSS for x.css"))).toBe(
    true
  )
  expect(isChunkLoadError(new Error("boom"))).toBe(false)
})
