import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { BrowserRouter } from "react-router-dom"
import "@openrails/billing-ui/admin.css"
import "./style.css"
import { bootstrapURL } from "./lib/mount"
import { ConsoleApp } from "./app"
import type { BootstrapConfig } from "./config"

const root = createRoot(document.getElementById("root")!)
fetch(bootstrapURL(), { cache: "no-store" })
  .then(async (response) => {
    if (!response.ok)
      throw new Error(`Console configuration failed (${response.status})`)
    const config = (await response.json()) as BootstrapConfig
    root.render(
      <StrictMode>
        <BrowserRouter basename="/admin">
          <ConsoleApp config={config} />
        </BrowserRouter>
      </StrictMode>
    )
  })
  .catch((error: unknown) =>
    root.render(
      <div role="alert">
        Admin console failed to start:{" "}
        {error instanceof Error ? error.message : String(error)}
      </div>
    )
  )
