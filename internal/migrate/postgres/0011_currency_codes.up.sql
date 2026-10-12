-- Repair: none-needed The check only widens to the registry's code shape, so every stored code still satisfies it.
ALTER TABLE billing.admission_operations
    DROP CONSTRAINT admission_operations_currency_check,
    ADD CONSTRAINT admission_operations_currency_check CHECK (currency ~ '^[A-Z0-9]{3,12}$');
