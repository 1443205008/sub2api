ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS rate_time_rules JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN groups.rate_time_rules IS
    '分组 token 计费的多时段叠加倍率规则，支持跨午夜；空数组表示仅使用基础倍率';
