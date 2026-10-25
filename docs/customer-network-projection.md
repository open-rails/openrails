# Merchant customer reads and platform account links

The merchant-scoped customer DTO contains contact, billing settings, balances and
default payment methods. Platform account links belong to the hosted product and
are read through its proven-link directory; ordinary customer reads must not
require that directory or expose a nullable compatibility projection.

This slice removes only the Customer.Network projection and its enrichment read.
It does not remove the existing core network linking routes, proof service,
lifecycle jobs or migration history; those remain until their SaaS replacements
are qualified. The generated public Go/HTTP/TypeScript customer shape changes.

The approved test seam is the real standalone server's public merchant Go client
and customer HTTP read/list APIs. A real consented legacy link is created first;
then only that isolated fixture's link table is renamed to model external
ownership. The reads must preserve the same customer/settings/balance and refuse
a different merchant's customer. No live database or provider is involved.
