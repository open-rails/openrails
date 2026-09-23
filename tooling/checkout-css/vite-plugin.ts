import type { Plugin } from "vite"

import { isolateCheckoutCss } from "./isolate.ts"

const STYLE_ELEMENT_ID = "billing-ui-styles"

/**
 * Vite extracts library CSS instead of retaining the source import. Installing
 * the isolated result from the JavaScript entry lets consumers import the
 * component alone; the emitted stylesheet remains available for SSR or manual
 * loading.
 */
export function renderStyleInstaller(css: string): string {
  return `
const __orckCss = ${JSON.stringify(css)};
if (typeof document !== "undefined") {
  let __orckStyle = document.getElementById(${JSON.stringify(STYLE_ELEMENT_ID)});
  if (!__orckStyle) {
    __orckStyle = document.createElement("style");
    __orckStyle.id = ${JSON.stringify(STYLE_ELEMENT_ID)};
    __orckStyle.setAttribute("data-billing-ui", "");
    (document.head || document.documentElement).appendChild(__orckStyle);
  }
  if (__orckStyle.textContent !== __orckCss) __orckStyle.textContent = __orckCss;
}
`
}

export function checkoutCssPlugin(options: { entries: string[] }): Plugin {
  return {
    name: "billing-ui-css",
    enforce: "post",
    async generateBundle(_options, bundle) {
      const stylesheet = Object.values(bundle).find(
        (item) => item.type === "asset" && item.fileName.endsWith(".css")
      )
      if (!stylesheet || stylesheet.type !== "asset") {
        throw new Error("billing-ui build did not emit a stylesheet")
      }

      const css = await isolateCheckoutCss(String(stylesheet.source))
      stylesheet.source = css

      for (const item of Object.values(bundle)) {
        if (
          item.type === "chunk" &&
          item.isEntry &&
          options.entries.includes(item.name)
        ) {
          item.code = `${renderStyleInstaller(css)}\n${item.code}`
        }
      }
    },
  }
}
