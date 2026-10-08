-- parent: 10 sha256:8e9399bd78359818da33b8b6c56576ce2590da531b286de02463faadd1f5855e
-- Repair: none-needed The check only widens to the registry's code shape, so every stored code still satisfies it.
ALTER TABLE billing.admission_operations
    DROP CONSTRAINT admission_operations_currency_check,
    ADD CONSTRAINT admission_operations_currency_check CHECK (currency ~ '^[A-Z0-9]{3,12}$');
