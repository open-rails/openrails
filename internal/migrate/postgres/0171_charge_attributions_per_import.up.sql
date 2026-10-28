-- Repair: none-needed Each import queues one job for the attributions it
-- declared, which reads them by key; nothing lists due attributions across
-- merchants.
DROP INDEX billing.provider_charge_attributions_due_idx;
