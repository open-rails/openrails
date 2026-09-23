import postcss, { type Plugin, type Rule } from "postcss"

const CHECKOUT_ROOT = ".orck"

// DialogContent places its backdrop beside the popup in a Base UI portal, so
// the backdrop cannot inherit the popup's `.orck` scope. This selector admits
// only the portal that owns an OpenRails checkout dialog.
const CHECKOUT_DIALOG_PORTAL =
  ':where([data-slot="dialog-portal"]:has(> .orck[data-slot="dialog-content"]))'

function removeCascadeLayers(): Plugin {
  return {
    postcssPlugin: "billing-ui-remove-layers",
    Once(root) {
      root.walkAtRules("layer", (rule) => {
        if (rule.nodes) {
          rule.replaceWith(...rule.nodes)
          return
        }

        rule.remove()
      })
    },
  }
}

function isKeyframeStep(rule: Rule): boolean {
  const parent = rule.parent
  return parent?.type === "atrule" && /keyframes$/i.test(parent.name)
}

function alreadyCheckoutScoped(selector: string): boolean {
  return (
    selector === CHECKOUT_ROOT ||
    selector.startsWith(`${CHECKOUT_ROOT}.`) ||
    selector.startsWith(`${CHECKOUT_ROOT}[`) ||
    selector.startsWith(`${CHECKOUT_ROOT}:`) ||
    selector.startsWith(`${CHECKOUT_ROOT} `) ||
    selector.startsWith(`${CHECKOUT_ROOT}-`)
  )
}

function sameElementSelector(selector: string): string | undefined {
  if (selector === "*") return CHECKOUT_ROOT
  if (selector.startsWith("*:")) {
    return `${CHECKOUT_ROOT}${selector.slice(1)}`
  }
  if (selector.startsWith(".") || selector.startsWith("[")) {
    return `${CHECKOUT_ROOT}${selector}`
  }
  if (/^::?(before|after|backdrop)$/.test(selector)) {
    return `${CHECKOUT_ROOT}${selector}`
  }
  return undefined
}

function scopeSelectors(): Plugin {
  return {
    postcssPlugin: "billing-ui-scope-selectors",
    Once(root) {
      root.walkRules((rule) => {
        if (isKeyframeStep(rule)) return

        const scoped = rule.selectors.flatMap((selector) => {
          if ([":root", ":host", "html", "body"].includes(selector)) {
            return [CHECKOUT_ROOT, CHECKOUT_DIALOG_PORTAL]
          }
          if (alreadyCheckoutScoped(selector)) return [selector]

          const selectors = [
            `${CHECKOUT_ROOT} ${selector}`,
            `${CHECKOUT_DIALOG_PORTAL} ${selector}`,
          ]
          const sameElement = sameElementSelector(selector)
          if (sameElement) selectors.push(sameElement)
          return selectors
        })

        rule.selectors = [...new Set(scoped)]
      })
    },
  }
}

function namespaceTailwindInternals(): Plugin {
  return {
    postcssPlugin: "billing-ui-namespace-tailwind-internals",
    Once(root) {
      root.walkAtRules((rule) => {
        if (/keyframes$/i.test(rule.name) && !rule.params.startsWith("orck-")) {
          rule.params = `orck-${rule.params}`
        }
        rule.params = rule.params.replaceAll("--tw-", "--orck-tw-")
      })
      root.walkDecls((declaration) => {
        declaration.prop = declaration.prop.replaceAll("--tw-", "--orck-tw-")
        declaration.value = declaration.value
          .replaceAll("--tw-", "--orck-tw-")
          .replace(/\b(spin|pulse|enter|exit)\b/g, "orck-$1")
      })
    },
  }
}

/**
 * Turns Tailwind's global library output into checkout-owned CSS.
 *
 * Layers are removed because a host's later layer can outrank a more specific
 * checkout selector. Selectors, Tailwind variables, and keyframes are then
 * namespaced so neither side can style the other accidentally.
 */
export async function isolateCheckoutCss(css: string): Promise<string> {
  const result = await postcss([
    removeCascadeLayers(),
    scopeSelectors(),
    namespaceTailwindInternals(),
  ]).process(css, { from: undefined })

  return result.css
}
