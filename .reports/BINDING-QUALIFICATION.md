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

Final published dependency qualification: AuthKit v0.114.0 and RiverKit v0.2.0 are pinned and resolved with GOWORK=off, without replacements. The private workspace file remains ignored and was unused for final validation. All-repository build and vet, contract generation/check, module verification, and focused core race tests passed. The final published-module integration rerun passed physical identity9.810s and public binding/migration/started-worker schema controls12.079s, including automatic no-skip second-cluster controls. Real standalone CLI startup passed5.724s; no unexpected external HTTP (24 background FX requests blocked).

This PR remains stacked on1856841; root's newer candidate includes separately owned fixture repairs#599/#600. No stale-base full CI was launched and none of those foreign changes were duplicated. Required final integration CI must run on the composed candidate including this correction and separately owned transition wakeups#598. Root owns integration/merge; this lane did not publish, tag, or merge dependencies.
