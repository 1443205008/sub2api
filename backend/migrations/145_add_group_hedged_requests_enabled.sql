-- 分组级 OpenAI /v1/responses 流式首 token 竞速开关。
-- 开启后，该分组下符合条件的 OpenAI API Key 账号会并发请求，首个产生客户端可见 token 的账号胜出。

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS hedged_requests_enabled BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN groups.hedged_requests_enabled IS
    'Enable OpenAI /v1/responses streaming hedged requests for this group; first visible token wins.';
