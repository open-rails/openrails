UPDATE billing.entitlements
SET source_type = 'one_off',
    updated_at = current_timestamp
WHERE source_type = 'payment';
