-- Migrate the legacy redeem-code based invite cashback data into the new
-- affiliate tables.
--
-- Legacy model:
--   - invite source code:
--       redeem_codes.type='invitation'
--       redeem_codes.notes='invite_issuer_user_id:<inviter_id>'
--   - invite usage/binding:
--       redeem_codes.type='invitation'
--       redeem_codes.used_by=<invitee_id>
--       redeem_codes.notes in (
--         'invite_issuer_user_id:<inviter_id>',
--         'invite_usage_issuer_user_id:<inviter_id>'
--       )
--   - cashback already credited directly to inviter balance:
--       redeem_codes.type='invite_cashback'
--       redeem_codes.used_by=<inviter_id>
--       redeem_codes.value=<cashback_amount>
--       redeem_codes.notes='invite_cashback_from_user_id:<invitee_id>'
--
-- New model:
--   - user_affiliates stores the inviter binding and aggregate counters.
--   - user_affiliate_ledger stores rebate accrual/transfer history.
--
-- Important: legacy cashback was already credited to user balance. Therefore
-- this migration imports legacy cashback as historical accrual ledger and
-- updates aff_history_quota, but intentionally does NOT add it to aff_quota or
-- aff_frozen_quota. This avoids allowing users to transfer the same rebate
-- into balance a second time.

-- Keep track of legacy redeem-code cashback rows already imported so this
-- migration remains safe if replayed in a partially migrated environment.
CREATE TABLE IF NOT EXISTS legacy_invite_cashback_migration (
    redeem_code_id BIGINT PRIMARY KEY REFERENCES redeem_codes(id) ON DELETE CASCADE,
    migrated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Carry old feature settings to the new affiliate settings.
INSERT INTO settings (key, value, updated_at)
SELECT 'affiliate_enabled', value, NOW()
FROM settings
WHERE key = 'invite_cashback_enabled'
  AND value IN ('true', 'false')
ON CONFLICT (key) DO UPDATE
SET value = EXCLUDED.value,
    updated_at = NOW();

INSERT INTO settings (key, value, updated_at)
SELECT 'affiliate_rebate_rate',
       to_char(LEAST(GREATEST(value::numeric, 0), 100), 'FM999999990.########'),
       NOW()
FROM settings
WHERE key = 'invite_cashback_rate'
  AND value ~ '^-?[0-9]+(\.[0-9]+)?$'
ON CONFLICT (key) DO UPDATE
SET value = EXCLUDED.value,
    updated_at = NOW();

-- Ensure affiliate rows exist for all users participating in legacy invites or
-- legacy cashback records. Use deterministic generated codes first, then
-- restore old reusable invite source codes for inviters where possible.
WITH legacy_source_codes AS (
    SELECT DISTINCT ON (src.inviter_id)
           src.inviter_id AS user_id,
           UPPER(src.code) AS legacy_code,
           src.created_at
    FROM (
        SELECT rc.id,
               rc.code,
               substring(COALESCE(rc.notes, '') FROM '^invite_issuer_user_id:([0-9]+)$')::BIGINT AS inviter_id,
               rc.created_at
        FROM redeem_codes rc
        WHERE rc.type = 'invitation'
          AND COALESCE(rc.notes, '') ~ '^invite_issuer_user_id:[0-9]+$'
    ) src
    JOIN users u ON u.id = src.inviter_id
    WHERE UPPER(src.code) ~ '^[A-Z0-9_-]{1,32}$'
    ORDER BY src.inviter_id, src.created_at DESC NULLS LAST, src.id DESC
),
legacy_invitation_bindings AS (
    SELECT rc.id AS source_id,
           rc.used_by AS invitee_id,
           COALESCE(
               substring(COALESCE(rc.notes, '') FROM '^invite_usage_issuer_user_id:([0-9]+)$'),
               substring(COALESCE(rc.notes, '') FROM '^invite_issuer_user_id:([0-9]+)$')
           )::BIGINT AS inviter_id,
           COALESCE(rc.used_at, rc.created_at) AS bound_at,
           0 AS priority
    FROM redeem_codes rc
    WHERE rc.type = 'invitation'
      AND rc.used_by IS NOT NULL
      AND COALESCE(rc.notes, '') ~ '^invite_(usage_)?issuer_user_id:[0-9]+$'
),
legacy_cashback_bindings AS (
    SELECT rc.id AS source_id,
           substring(COALESCE(rc.notes, '') FROM '^invite_cashback_from_user_id:([0-9]+)$')::BIGINT AS invitee_id,
           rc.used_by AS inviter_id,
           COALESCE(rc.used_at, rc.created_at) AS bound_at,
           1 AS priority
    FROM redeem_codes rc
    WHERE rc.type = 'invite_cashback'
      AND rc.status = 'used'
      AND rc.used_by IS NOT NULL
      AND rc.value > 0
      AND COALESCE(rc.notes, '') ~ '^invite_cashback_from_user_id:[0-9]+$'
),
legacy_bindings AS (
    SELECT DISTINCT ON (invitee_id)
           invitee_id,
           inviter_id,
           bound_at
    FROM (
        SELECT * FROM legacy_invitation_bindings
        UNION ALL
        SELECT * FROM legacy_cashback_bindings
    ) b
    JOIN users invitee ON invitee.id = b.invitee_id
    JOIN users inviter ON inviter.id = b.inviter_id
    WHERE b.invitee_id > 0
      AND b.inviter_id > 0
      AND b.invitee_id <> b.inviter_id
    ORDER BY invitee_id, priority ASC, bound_at DESC NULLS LAST, source_id DESC
),
related_users AS (
    SELECT user_id FROM legacy_source_codes
    UNION
    SELECT inviter_id AS user_id FROM legacy_bindings
    UNION
    SELECT invitee_id AS user_id FROM legacy_bindings
)
INSERT INTO user_affiliates (user_id, aff_code, created_at, updated_at)
SELECT ru.user_id,
       'L' || UPPER(substring(md5(ru.user_id::TEXT || ':legacy-affiliate') FROM 1 FOR 31)),
       COALESCE(u.created_at, NOW()),
       NOW()
FROM related_users ru
JOIN users u ON u.id = ru.user_id
ON CONFLICT DO NOTHING;

-- Preserve old reusable invite codes as the new affiliate code for inviters.
-- If a code is already occupied by another affiliate row, keep the generated
-- code instead to avoid violating the unique constraint.
WITH legacy_source_codes AS (
    SELECT DISTINCT ON (src.inviter_id)
           src.inviter_id AS user_id,
           UPPER(src.code) AS legacy_code,
           src.created_at
    FROM (
        SELECT rc.id,
               rc.code,
               substring(COALESCE(rc.notes, '') FROM '^invite_issuer_user_id:([0-9]+)$')::BIGINT AS inviter_id,
               rc.created_at
        FROM redeem_codes rc
        WHERE rc.type = 'invitation'
          AND COALESCE(rc.notes, '') ~ '^invite_issuer_user_id:[0-9]+$'
    ) src
    JOIN users u ON u.id = src.inviter_id
    WHERE UPPER(src.code) ~ '^[A-Z0-9_-]{1,32}$'
    ORDER BY src.inviter_id, src.created_at DESC NULLS LAST, src.id DESC
)
UPDATE user_affiliates ua
SET aff_code = lc.legacy_code,
    aff_code_custom = TRUE,
    updated_at = NOW()
FROM legacy_source_codes lc
WHERE ua.user_id = lc.user_id
  AND NOT EXISTS (
      SELECT 1
      FROM user_affiliates occupied
      WHERE occupied.aff_code = lc.legacy_code
        AND occupied.user_id <> ua.user_id
  )
  AND (
      ua.aff_code <> lc.legacy_code
      OR ua.aff_code_custom IS DISTINCT FROM TRUE
  );

-- Migrate inviter bindings. Existing non-null inviter_id wins so that this
-- migration does not overwrite relations created after switching to the new
-- affiliate implementation.
WITH legacy_invitation_bindings AS (
    SELECT rc.id AS source_id,
           rc.used_by AS invitee_id,
           COALESCE(
               substring(COALESCE(rc.notes, '') FROM '^invite_usage_issuer_user_id:([0-9]+)$'),
               substring(COALESCE(rc.notes, '') FROM '^invite_issuer_user_id:([0-9]+)$')
           )::BIGINT AS inviter_id,
           COALESCE(rc.used_at, rc.created_at) AS bound_at,
           0 AS priority
    FROM redeem_codes rc
    WHERE rc.type = 'invitation'
      AND rc.used_by IS NOT NULL
      AND COALESCE(rc.notes, '') ~ '^invite_(usage_)?issuer_user_id:[0-9]+$'
),
legacy_cashback_bindings AS (
    SELECT rc.id AS source_id,
           substring(COALESCE(rc.notes, '') FROM '^invite_cashback_from_user_id:([0-9]+)$')::BIGINT AS invitee_id,
           rc.used_by AS inviter_id,
           COALESCE(rc.used_at, rc.created_at) AS bound_at,
           1 AS priority
    FROM redeem_codes rc
    WHERE rc.type = 'invite_cashback'
      AND rc.status = 'used'
      AND rc.used_by IS NOT NULL
      AND rc.value > 0
      AND COALESCE(rc.notes, '') ~ '^invite_cashback_from_user_id:[0-9]+$'
),
legacy_bindings AS (
    SELECT DISTINCT ON (invitee_id)
           invitee_id,
           inviter_id,
           bound_at
    FROM (
        SELECT * FROM legacy_invitation_bindings
        UNION ALL
        SELECT * FROM legacy_cashback_bindings
    ) b
    JOIN users invitee ON invitee.id = b.invitee_id
    JOIN users inviter ON inviter.id = b.inviter_id
    WHERE b.invitee_id > 0
      AND b.inviter_id > 0
      AND b.invitee_id <> b.inviter_id
    ORDER BY invitee_id, priority ASC, bound_at DESC NULLS LAST, source_id DESC
)
UPDATE user_affiliates invitee_aff
SET inviter_id = lb.inviter_id,
    updated_at = NOW()
FROM legacy_bindings lb
WHERE invitee_aff.user_id = lb.invitee_id
  AND invitee_aff.inviter_id IS NULL;

-- Recompute invitation counts from actual bindings.
UPDATE user_affiliates ua
SET aff_count = counts.cnt,
    updated_at = NOW()
FROM (
    SELECT inviter.user_id,
           COUNT(invitee.user_id)::INTEGER AS cnt
    FROM user_affiliates inviter
    LEFT JOIN user_affiliates invitee ON invitee.inviter_id = inviter.user_id
    GROUP BY inviter.user_id
) counts
WHERE ua.user_id = counts.user_id
  AND ua.aff_count <> counts.cnt;

-- Import legacy cashback rows as historical accrual ledger entries.
WITH legacy_cashbacks_raw AS (
    SELECT rc.id AS redeem_code_id,
           rc.used_by AS inviter_id,
           substring(COALESCE(rc.notes, '') FROM '^invite_cashback_from_user_id:([0-9]+)$')::BIGINT AS invited_user_id,
           rc.value::NUMERIC(20,8) AS amount,
           COALESCE(rc.used_at, rc.created_at, NOW()) AS occurred_at
    FROM redeem_codes rc
    WHERE rc.type = 'invite_cashback'
      AND rc.status = 'used'
      AND rc.used_by IS NOT NULL
      AND rc.value > 0
      AND COALESCE(rc.notes, '') ~ '^invite_cashback_from_user_id:[0-9]+$'
),
legacy_cashbacks AS (
    SELECT lcr.*
    FROM legacy_cashbacks_raw lcr
    JOIN users inviter ON inviter.id = lcr.inviter_id
    JOIN users invitee ON invitee.id = lcr.invited_user_id
    JOIN user_affiliates inviter_aff ON inviter_aff.user_id = lcr.inviter_id
    WHERE lcr.inviter_id <> lcr.invited_user_id
      AND NOT EXISTS (
          SELECT 1
          FROM legacy_invite_cashback_migration done
          WHERE done.redeem_code_id = lcr.redeem_code_id
      )
      AND NOT EXISTS (
          SELECT 1
          FROM user_affiliate_ledger existing
          WHERE existing.user_id = lcr.inviter_id
            AND existing.action = 'accrue'
            AND existing.source_user_id = lcr.invited_user_id
            AND existing.amount = lcr.amount
            AND existing.created_at = lcr.occurred_at
      )
),
marked AS (
    INSERT INTO legacy_invite_cashback_migration (redeem_code_id)
    SELECT redeem_code_id
    FROM legacy_cashbacks
    ON CONFLICT DO NOTHING
    RETURNING redeem_code_id
)
INSERT INTO user_affiliate_ledger (
    user_id,
    action,
    amount,
    source_user_id,
    frozen_until,
    created_at,
    updated_at
)
SELECT lc.inviter_id,
       'accrue',
       lc.amount,
       lc.invited_user_id,
       NULL,
       lc.occurred_at,
       lc.occurred_at
FROM legacy_cashbacks lc
JOIN marked m ON m.redeem_code_id = lc.redeem_code_id;

-- Reflect imported historical accruals in aggregate history without making
-- already-paid legacy cashback available for transfer again.
WITH accrued AS (
    SELECT user_id,
           COALESCE(SUM(amount), 0)::NUMERIC(20,8) AS total_accrued
    FROM user_affiliate_ledger
    WHERE action = 'accrue'
    GROUP BY user_id
)
UPDATE user_affiliates ua
SET aff_history_quota = GREATEST(ua.aff_history_quota, accrued.total_accrued),
    updated_at = NOW()
FROM accrued
WHERE ua.user_id = accrued.user_id
  AND ua.aff_history_quota < accrued.total_accrued;
