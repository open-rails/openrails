DO $$
DECLARE
    duplicate_identities text;
BEGIN
    SELECT string_agg(
        format(
            '(rail=%s, environment=%s, account_id=%s, merchants=%s)',
            rail,
            environment,
            account_id,
            merchant_ids
        ),
        '; '
        ORDER BY rail, environment, account_id
    )
    INTO duplicate_identities
    FROM (
        SELECT
            rail,
            environment,
            account_id,
            array_agg(merchant_id ORDER BY merchant_id)::text AS merchant_ids
        FROM openrails.payment_provider_accounts
        GROUP BY rail, environment, account_id
        HAVING count(*) > 1
    ) duplicates;

    IF duplicate_identities IS NOT NULL THEN
        RAISE EXCEPTION 'duplicate payment provider account identities: %', duplicate_identities;
    END IF;
END $$;

DROP INDEX IF EXISTS openrails.uq_payment_provider_accounts_identity;

CREATE UNIQUE INDEX uq_payment_provider_accounts_identity
    ON openrails.payment_provider_accounts (rail, environment, account_id);
