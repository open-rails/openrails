import { act } from "react"
import { createRoot, type Root } from "react-dom/client"
import { afterEach, beforeEach, expect, it, vi } from "vitest"
import type { AuthClient } from "@openrails/auth-ui/client"

import { InvitationPage } from "./account-links"

const state = vi.hoisted(() => ({ status: "signed_out" }))
vi.mock("../lib/identity", () => ({
  useIdentity: () => ({
    status: state.status,
    user: { email: "new@example.test" },
  }),
}))
vi.mock("@openrails/auth-ui", () => ({
  ResetPasswordForm: () => null,
  VerifyLink: () => null,
  SignInPanel: ({
    onStepChange,
    onSignedIn,
  }: {
    onStepChange: (step: "register") => void
    onSignedIn: () => void
  }) => (
    <button
      onClick={() => {
        onStepChange("register")
        onSignedIn()
      }}
    >
      Complete registration
    </button>
  ),
}))

let root: Root
let element: HTMLDivElement
beforeEach(() => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true)
  state.status = "signed_out"
  element = document.createElement("div")
  document.body.append(element)
  root = createRoot(element)
})
afterEach(async () => {
  await act(async () => root.unmount())
  element.remove()
  window.history.replaceState(null, "", "/admin/invite")
  vi.unstubAllGlobals()
})

it.each(["code", "invite_code"] as const)(
  "%s invitation completes only when its membership grant is consumed",
  async (parameter) => {
    window.history.replaceState(
      null,
      "",
      `/admin/invite#${parameter}=team-code`
    )
    const onDone = vi.fn()
    const redeem = vi.fn().mockResolvedValue(undefined)
    const auth = { redeemInvitation: redeem } as unknown as AuthClient
    const render = () =>
      root.render(<InvitationPage auth={auth} onDone={onDone} />)
    await act(async () => render())
    expect(window.location.hash).toBe("")
    await act(async () => element.querySelector("button")!.click())

    expect(redeem).not.toHaveBeenCalled()
    if (parameter === "invite_code") {
      // AuthKit consumes a registration invitation with signup itself.
      expect(onDone).toHaveBeenCalledOnce()
      return
    }
    // Registering an account does not accept a generic team invitation.
    expect(onDone).not.toHaveBeenCalled()
    state.status = "signed_in"
    await act(async () => render())
    const accept = [...element.querySelectorAll("button")].find(
      (button) => button.textContent === "Accept invitation"
    )
    expect(accept).toBeDefined()
    await act(async () => accept!.click())
    expect(redeem).toHaveBeenCalledExactlyOnceWith("team-code")
    expect(onDone).toHaveBeenCalledOnce()
  }
)
