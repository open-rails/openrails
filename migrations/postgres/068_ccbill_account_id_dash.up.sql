-- #697: CCBill composite account identity is dash-joined, matching CCBill's own
-- convention: clientAccnum-clientSubacc (e.g. 945280-0000). The old slash form
-- (945280/0000) also re-embedded the merchant-secret path delimiter inside the
-- id (canonical name shape: rail_merchant_accounts/<rail>/<env>/<account_id>/<field>),
-- which forced URL-escaping (%2F) in stored secret names. Hard cut, forward-only
-- (059 pattern). Vault-backed deployments must move any KV entries whose ccbill
-- account-id segment contains %2F (or a raw slash) to the dash form at upgrade
-- (operator step; there are none pre-launch).

-- --- declared rail identity ---
UPDATE openrails.rail_merchant_accounts
   SET account_id = replace(account_id, '/', '-')
 WHERE rail = 'ccbill'
   AND account_id LIKE '%/%';

-- --- stored secret names (DB-backed store + audit trail) ---
-- Canonical writer URL-escapes the account-id segment, so slash-form ccbill ids
-- were stored as 5-segment names with %2F inside segment 4:
--   rail_merchant_accounts/ccbill/<env>/<acc>%2F<sub>/<field>
UPDATE openrails.merchant_secrets
   SET name = split_part(name, '/', 1) || '/' || split_part(name, '/', 2) || '/'
           || split_part(name, '/', 3) || '/'
           || replace(replace(split_part(name, '/', 4), '%2F', '-'), '%2f', '-') || '/'
           || split_part(name, '/', 5)
 WHERE name LIKE 'rail_merchant_accounts/ccbill/%'
   AND array_length(string_to_array(name, '/'), 1) = 5
   AND (position('%2F' in split_part(name, '/', 4)) > 0
     OR position('%2f' in split_part(name, '/', 4)) > 0);

UPDATE openrails.merchant_credential_audit
   SET name = split_part(name, '/', 1) || '/' || split_part(name, '/', 2) || '/'
           || split_part(name, '/', 3) || '/'
           || replace(replace(split_part(name, '/', 4), '%2F', '-'), '%2f', '-') || '/'
           || split_part(name, '/', 5)
 WHERE name LIKE 'rail_merchant_accounts/ccbill/%'
   AND array_length(string_to_array(name, '/'), 1) = 5
   AND (position('%2F' in split_part(name, '/', 4)) > 0
     OR position('%2f' in split_part(name, '/', 4)) > 0);

-- Defensive: any name written with a RAW slash inside the ccbill account id has
-- one extra segment (6 total) — collapse the two id segments into one dash-joined
-- segment. Only ccbill names are eligible; no other rail's account_id ever
-- contained the delimiter.
UPDATE openrails.merchant_secrets
   SET name = split_part(name, '/', 1) || '/' || split_part(name, '/', 2) || '/'
           || split_part(name, '/', 3) || '/'
           || split_part(name, '/', 4) || '-' || split_part(name, '/', 5) || '/'
           || split_part(name, '/', 6)
 WHERE name LIKE 'rail_merchant_accounts/ccbill/%'
   AND array_length(string_to_array(name, '/'), 1) = 6;

UPDATE openrails.merchant_credential_audit
   SET name = split_part(name, '/', 1) || '/' || split_part(name, '/', 2) || '/'
           || split_part(name, '/', 3) || '/'
           || split_part(name, '/', 4) || '-' || split_part(name, '/', 5) || '/'
           || split_part(name, '/', 6)
 WHERE name LIKE 'rail_merchant_accounts/ccbill/%'
   AND array_length(string_to_array(name, '/'), 1) = 6;
