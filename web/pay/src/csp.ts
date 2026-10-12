// The page's CSP has no 'unsafe-eval'. zod probes eval with Function("") as
// each schema is built, which the page reports as a violation; this runs
// before billing-ui's schemas exist, so zod never probes.
import { config } from "zod"

config({ jitless: true })
