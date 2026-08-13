// Buttons, links and fields inside a clickable container own their own clicks;
// the container must not act on a click that was meant for them.
export function isInteractiveTarget(target: EventTarget | null): boolean {
  return Boolean(
    target instanceof Element &&
    target.closest("a,button,input,select,textarea,[role='menuitem']")
  )
}
