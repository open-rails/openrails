# Configured HTTP route bundles

Tracker: https://github.com/open-rails/tracker/blob/master/openrails/1033.md
Owner: Codex /root/astra_configured_routes
Base: 50f24a0074605fa4d9663870eb1d51f108840f31

Configure HTTP exposure when constructing the embedded runtime, or call its
one-shot ConfigureHTTP after merchant provisioning supplies the verified identity
binding. Requesting routes freezes policy; reconfiguration and use after Close
are refused. Materialize one
route bundle and mount it once on the host router. An unconfigured runtime refuses HTTP route construction. An enabled HTTP configuration includes capability discovery and callbacks
for configured providers; buyer and management HTTP surfaces require explicit
configuration. In-process catalog access is independent of HTTP exposure.

The neutral route registration functions remain authoritative. net/http (including
Chi), Gin and Fiber adapters register individual method/path handlers and preserve
raw bodies, authorization, path parameters and host-selected prefixes. They reuse
the initialized runtime and do not create workers, servers or database pools.

Consumer census at the source base: demo mounts only provider callbacks; Doujins
and Hentai0 mount checkout, customer, merchant administration and catalog plus
callbacks using their delegated authenticators/gates. SaaS is on an older public
facade and has separate standalone/control-plane and callback/self mounts; its
migration requires preserving platform/control-plane ownership. These distinctions
must become explicit constructor configuration, not a broad library default.

Implementation and qualification are in progress. Canonical billing PR603 is
merged and included; its owner retains release v0.148.0. This task qualifies a
separate routing release before published consumer adoption.
