ALTER TABLE hosts
	ADD COLUMN IF NOT EXISTS gateway_config_mode TEXT NOT NULL DEFAULT 'auto';

DO $$
BEGIN
	ALTER TABLE hosts
		ADD CONSTRAINT hosts_gateway_config_mode_check
		CHECK (gateway_config_mode IN ('auto', 'custom'));
EXCEPTION
	WHEN duplicate_object THEN NULL;
END $$;

UPDATE hosts
SET gateway_config_mode = 'custom'
WHERE gateway_config IS NOT NULL
	AND jsonb_typeof(gateway_config) = 'object'
	AND (
		gateway_config ? 'inbounds'
		OR gateway_config ? 'outbounds'
		OR gateway_config ? 'route'
	);
