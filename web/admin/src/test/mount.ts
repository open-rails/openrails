// Test-only jsdom scaffolding for the workflows that need a live DOM: the
// browser APIs Base UI expects, an act-wrapped mount, and label-addressed
// controls. Kept out of harness.ts so node-environment tests never load
// react-dom/client.
import { act } from "react"
import { createRoot, type Root } from "react-dom/client"
import { notifyManager } from "@tanstack/react-query"
import { expect, vi } from "vitest"
import type { ReactNode } from "react"

let root: Root | undefined

export function browserEnvironment() {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true)
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    }
  )
  window.matchMedia = vi.fn().mockImplementation(() => ({
    matches: false,
    addEventListener() {},
    removeEventListener() {},
  }))
  // Query updates land inside the act() call that caused them.
  notifyManager.setScheduler(queueMicrotask)
}

export async function mount(node: ReactNode) {
  const container = document.createElement("div")
  document.body.append(container)
  root = createRoot(container)
  await act(async () => root!.render(node))
}

export async function unmount() {
  if (root) await act(async () => root!.unmount())
  root = undefined
  document.body.innerHTML = ""
  notifyManager.setScheduler((callback) => setTimeout(callback, 0))
  vi.unstubAllGlobals()
}

export function button(label: string) {
  const found = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
    (node) => node.textContent === label
  )
  expect(found, `button ${label}`).toBeDefined()
  expect(found!.disabled).toBe(false)
  return found!
}

export const click = (label: string) => act(async () => button(label).click())

// Opens the only listbox on screen and picks the option starting with `prefix`.
export async function choose(prefix: string) {
  await act(async () =>
    document.querySelector<HTMLButtonElement>('[role="combobox"]')!.click()
  )
  const option = [
    ...document.querySelectorAll<HTMLElement>('[role="option"]'),
  ].find((node) => node.textContent?.startsWith(prefix))
  expect(option, `option ${prefix}`).toBeDefined()
  await act(async () => option!.click())
}

export { act }
