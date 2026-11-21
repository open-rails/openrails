ALTER TABLE billing.psps
    ADD COLUMN write_mode text NOT NULL DEFAULT 'readonly',
    ADD CONSTRAINT psps_write_mode_check CHECK (write_mode IN ('readonly', 'limited', 'full'));
