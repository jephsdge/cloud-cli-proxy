-- Use Eastern Time as the default for newly created hosts.
ALTER TABLE hosts ALTER COLUMN timezone SET DEFAULT 'America/New_York';
