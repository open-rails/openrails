# PR590 response-boundary correction

Owner: /root/astra_nmi_response_finish, replacing /root/astra_engine_cancel with dirty test WIP preserved.
Base: cb37d163a159a7dd4c8c178b4888de2659aa94f8; branch fix/297-nmi-response-boundary-20260921.

Classic replies reject repeated fields, contradictory response/code pairs, malformed encoding, and missing or invalid decline codes. Only coherent response=2 with a 2xx processor code produces CustomerVaultError. Unknown outcomes have controlled diagnostics and no decline-typed error in their chain. Existing approval replies without a processor code remain supported for classic plan replies; receipt qualification remains the financial success authority.

Initial-membership refusal custody reparses via existing ParseSaleResponse and accepts only the same qualified decline. RawResponse remains internal to qualified refusal proof; provider text is omitted from Message and ordinary diagnostics. v5 HTTP diagnostics retain status only. HyperSwitch translates NMI uncertainty to its existing ErrUnknown contract before any decline classification.

Local qualification:
- Race tests: NMI, intents, direct NMI rail, Basis Theory proxy rail, HyperSwitch rail. Logs: nmi-boundary-final-race.log.
- Guarded PostgreSQL race tests: initial enrollment response boundary, accepted modes, and raw-progress authority; passed (19.938s), zero external HTTP attempts. Log: nmi-boundary-typed-postgres.log.
- The PostgreSQL regression checks unknown status, persisted failure reason, refusal-custody rejection, and one submission after verification. Qualified-refusal/approval existing fixtures pass.
- Focused lint: nmi-boundary-final-lint.log.

No live provider calls, deployment, tag, merge, shared-engine implementation edits, or foreign-worktree edits. Exact remote CI is separately required; root owns review and merge.
