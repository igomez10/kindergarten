-- A user-facing explanation of the current status ("Waiting for storage to be
-- provisioned."), separate from last_error, which is only set when
-- reconciliation itself failed.
ALTER TABLE instances ADD COLUMN status_message TEXT;
