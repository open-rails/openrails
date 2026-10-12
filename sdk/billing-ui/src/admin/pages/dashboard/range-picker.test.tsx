// @vitest-environment jsdom
import { fireEvent, screen } from "@testing-library/react"
import { useState } from "react"
import { afterEach, beforeEach, expect, it, vi } from "vitest"
import { AdminRouter } from "#orck/admin/router"
import { act, browserEnvironment, mount, unmount } from "#orck/admin/test/mount"
import { RangePicker } from "./range-picker"

beforeEach(browserEnvironment)
afterEach(unmount)

it("keeps both date edits before the host publishes its location, then follows external navigation", async () => {
  const navigate = vi.fn()
  let publishLocation: (search: string) => void = () => {}
  function Host() {
    const [search, setSearch] = useState("?from=2026-09-13&to=2026-10-12")
    publishLocation = setSearch
    return (
      <AdminRouter
        basePath="/billing"
        navigation={{ pathname: "/billing/", search, navigate }}
      >
        <RangePicker />
      </AdminRouter>
    )
  }
  await mount(<Host />)
  fireEvent.change(screen.getByLabelText("Start"), {
    target: { value: "2026-10-01" },
  })
  expect(navigate).toHaveBeenLastCalledWith(
    "/billing/?from=2026-10-01&to=2026-10-12",
    { replace: true }
  )
  fireEvent.change(screen.getByLabelText("End"), {
    target: { value: "2026-10-11" },
  })
  expect(navigate).toHaveBeenLastCalledWith(
    "/billing/?from=2026-10-01&to=2026-10-11",
    { replace: true }
  )
  await act(async () => publishLocation("?from=2026-09-01&to=2026-09-30"))
  expect(screen.getByLabelText("Start")).toHaveValue("2026-09-01")
  expect(screen.getByLabelText("End")).toHaveValue("2026-09-30")
})
