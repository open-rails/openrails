# Physical queue binding correction

Owner: /root/astra_nmi_response_finish. Core base1856841ef5b8e7e0443a82375a1fa2008a84c2d7. Branch fix/592-physical-queue-binding-20260922. No Store transition/wakeup changes.

RiverKit supplies Binding{Client,Pool}, preserving the actual borrowed host pool. Its omitted schema now means explicit public; custom schemas remain explicit. OpenRails validates physical queue/billing database identity and qualified river_job presence before exposing managed or host-composed producers. Supported transaction/pin contexts refuse composition immediately; normal admission reuses its already bound transaction without another pool acquisition. The migration/runtime pool boundary uses the same identity proof before DDL.

Identity uses two independently random transaction advisory lock keys plus exact holder backend and current database in pg_catalog.pg_locks. It compares no DSN/hostname/database-name strings. The temporary proof transaction rolls back and borrowed connections release on success/error/cancellation. No probe row/table/job, persisted identifier, reflection, or unstable River driver accessor exists. See PostgreSQL lock semantics: https://www.postgresql.org/docs/current/view-pg-locks.html .

Fresh qualification, GOMAXPROCS=2 and -p1:
- Physical identity race suite: same pool with max1 connection already in a transaction; distinct roles/aliases same physical DB; different DB with matching public queue; same DB name on another cluster; cancellation with zero remaining proof locks or borrowed connections. PASS5.940s.
- Ordinary-E2E automatic second-cluster path, without a manually supplied alternate DSN: helper6.656s; migration negative10.405s. Uses existing testcontainers pattern and exact owned teardown. Critical negatives do not skip in ordinary integration runs.
- Public binding/migration/composition controls: PASS38.748s, including no ledger commit after wrong DB/missing queue/binding-under-transaction/pin refusal, same-pool max1 admission rollback, and migration refusal before any schema mutation.
- Real started worker plus InsertTx using divergent role search_paths in public, canonical openrails and custom River schemas: PASS3.053s; uncommitted jobs invisible, committed jobs worked/completed, host pools remain usable.
- Real standalone run-server entrypoint, fresh migrated fixture and loopback NMI probe: PASS5.167s. Supported migrations provision River before buildRuntime's producer existence check. No live provider requests.
- Focused core unit/race packages pass; contract generation/check matches; focused lint passes.

Process HTTP guard reports zero unexpected external requests. Standalone's24 background FX attempts were blocked. Tests own isolated DBs/clusters; no foreign worktree or fixture cleanup.

Dependency state at this checkpoint: published RiverKit v0.2.0 is pinned. AuthKit adapter a1cffc64cf05e4a990428743347932e50ec4a0b1 is under root CI/release review for v0.114.0. Core qualification currently uses the private ignored .reports/go.work selecting that owned AuthKit source. No replace/workspace is committed. GOWORK=off qualification and full required CI remain after the published AuthKit pin; this checkpoint does not claim distributable-head completion.
