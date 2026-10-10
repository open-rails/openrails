// The console runs wherever the server mounts it: the Go handler points
// index.html's <base href> at the mount, so every console URL derives from
// document.baseURI, never from a literal path.

// routerBasename is the mount path without its trailing slash, e.g.
// "/billing/admin".
export function routerBasename(baseURI: string = document.baseURI): string {
  return new URL(".", baseURI).pathname.replace(/\/$/, "") || "/"
}

// bootstrapURL is the mount's config.json.
export function bootstrapURL(baseURI: string = document.baseURI): string {
  return new URL("config.json", baseURI).href
}

// normalizeLink maps an alert's stored link to an in-app router path or an
// external URL: { path } for router navigation, { href } for a new tab.
export function normalizeLink(
  link?: string | null,
  baseURI: string = document.baseURI
): { path?: string; href?: string } {
  if (!link) return {}
  if (/^https?:\/\//i.test(link)) return { href: link }
  const base = routerBasename(baseURI)
  let path = link
  if (base !== "/" && path.startsWith(`${base}/`)) path = path.slice(base.length)
  else if (path === base) path = "/"
  if (!path.startsWith("/")) path = `/${path}`
  return { path }
}
