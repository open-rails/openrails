// AuthKit's step-up dialog for the whole console, loaded apart from the entry
// chunk: it binds auth-ui's guard to AuthKit writes such as creating API
// keys or granting roles. OpenRails admin writes never use this guard.
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
