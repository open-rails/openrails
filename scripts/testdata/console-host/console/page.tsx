import { useConsole } from "@openrails/console"

import { label } from "@/lib/label"

export function FixturePage() {
  const { me } = useConsole("fixture")
  return (
    <p className="bg-[#0a1b2c]">
      {label} for {me?.email}
    </p>
  )
}
