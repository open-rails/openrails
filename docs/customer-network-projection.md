# Merchant customer reads and platform account links

The merchant-scoped customer DTO contains contact, billing settings, balances and
default payment methods. Platform account links belong to the hosted product and
are read through its proven-link directory; ordinary customer reads must not
require that directory or expose a nullable compatibility projection.

The generic DTO has no Customer.Network compatibility projection. The later
hosted-account extraction removes the old core service/routes/storage only as
part of the coordinated SaaS ownership handoff; migration history remains
immutable. See [the extraction plan](customer-network-removal-plan.md).

The approved test seam is the real standalone server's public merchant Go client
and customer HTTP read/list APIs. A real consented legacy link is created first;
then only that isolated fixture's link table is renamed to model external
ownership. The reads must preserve the same customer/settings/balance and refuse
a different merchant's customer. No live database or provider is involved.
