ALTER TABLE users
    ADD COLUMN IF NOT EXISTS registration_ip VARCHAR(45) NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_users_registration_ip
    ON users (registration_ip)
    WHERE deleted_at IS NULL AND registration_ip <> '';
