// A host's console extension, built by scripts/check.sh to keep the host build
// path (docs/admin-console.md, "Extending the console") working.
import { defineConsoleExtension } from "@openrails/console"

export default [
  defineConsoleExtension({
    id: "fixture",
    routes: [
      {
        path: "/fixture",
        scope: "user",
        lazy: () =>
          import("./page").then((module) => ({ Component: module.FixturePage })),
      },
    ],
    nav: [{ title: "Fixture", path: "/fixture", scope: "user" }],
  }),
]
