import js from "@eslint/js"
import globals from "globals"
import reactHooks from "eslint-plugin-react-hooks"
import reactRefresh from "eslint-plugin-react-refresh"
import pluginQuery from "@tanstack/eslint-plugin-query"
import tseslint from "typescript-eslint"
import { defineConfig, globalIgnores } from "eslint/config"

export default defineConfig([
  // Stock shadcn components/hooks are vendored as-is; don't lint them.
  globalIgnores(["dist", "src/components/ui/**", "src/hooks/use-mobile.ts"]),
  {
    files: ["**/*.{ts,tsx}"],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
      pluginQuery.configs["flat/recommended"],
    ],
    languageOptions: {
      globals: globals.browser,
    },
  },
  // Mutation callbacks run after the request, when the selected merchant may
  // have changed. mutations.ts must pin it with merchantQueryKeys() instead.
  {
    files: ["src/lib/mutations.ts"],
    rules: {
      "no-restricted-imports": [
        "error",
        {
          paths: [
            {
              name: "@/lib/queries",
              importNames: ["queryKeys"],
              message:
                "Use merchantQueryKeys() so cache keys name the merchant the mutation started under.",
            },
          ],
        },
      ],
    },
  },
])
