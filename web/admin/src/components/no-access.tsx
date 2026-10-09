// NoAccess is a merchant page the signed-in staff member may not use: none of
// the console's areas, or not this one.
export function NoAccess({ area }: { area?: string }) {
  return (
    <div className="flex items-center justify-center py-16">
      <div className="w-full max-w-sm">
        <h1 className="text-2xl font-semibold tracking-tight text-balance">
          {area
            ? `You don't have access to ${area}`
            : "You don't have access to this merchant's billing"}
        </h1>
        <p className="mt-2 text-sm text-pretty text-muted-foreground">
          Ask an administrator for a role that includes it.
        </p>
      </div>
    </div>
  )
}
