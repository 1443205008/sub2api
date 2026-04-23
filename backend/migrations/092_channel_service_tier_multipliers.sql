-- 渠道模型定价增加服务档位倍率配置
-- Standard: 默认/standard 档位
-- Fast: priority/fast 档位

ALTER TABLE channel_model_pricing
    ADD COLUMN IF NOT EXISTS service_tier_standard_multiplier NUMERIC(20,10),
    ADD COLUMN IF NOT EXISTS service_tier_fast_multiplier NUMERIC(20,10);

COMMENT ON COLUMN channel_model_pricing.service_tier_standard_multiplier IS '渠道模型定价：Standard/default 档位倍率，NULL 表示沿用系统默认行为';
COMMENT ON COLUMN channel_model_pricing.service_tier_fast_multiplier IS '渠道模型定价：Fast/priority 档位倍率，NULL 表示沿用系统默认行为';
