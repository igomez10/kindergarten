-- The LoadBalancer IP behind endpoint_host. Returned to clients as libpq's
-- hostaddr, so a connection does not depend on DNS propagation while
-- sslmode=verify-full still checks the certificate against endpoint_host.
ALTER TABLE instances ADD COLUMN endpoint_address TEXT;
