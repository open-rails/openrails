// AuthKit's step-up dialog for the whole console, loaded apart from the entry
// chunk: it binds auth-ui's guard to the api client, so every write OpenRails
// refuses with step_up_required opens it and runs again once confirmed.
import * as React from "react"
import { StepUpProvider, useStepUpGuard } from "@openrails/auth-ui"

import { bindStepUp } from "@/lib/api/client"

function StepUpBridge() {
  const guard = useStepUpGuard()
  React.useEffect(() => {
    bindStepUp(guard)
    return () => bindStepUp(null)
  }, [guard])
  return null
}

export default function StepUpHost() {
  return (
    <StepUpProvider>
      <StepUpBridge />
    </StepUpProvider>
  )
}
