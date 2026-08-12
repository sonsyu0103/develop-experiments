-- =============================================================================
-- 000005: 冪等キー (クライアント側のリトライを扱う)
-- =============================================================================
-- 判断の記録は docs/adr/0015-idempotency.md。
--
-- Phase 2 はサーバ内部のリトライ (直列化失敗) を主題にしているが、
-- **クライアント側のリトライを放置すると非対称になる。**
-- タイムアウト後の再送・回線の切り替え・複数タブ・送信直後のリロードは、
-- どれも「サーバでは成功していてコメントが 2 件できる」を起こす。
-- =============================================================================

CREATE TABLE idempotency_keys (
    -- 主キーを (user_id, key) にする。
    --
    -- **キーはクライアントが生成するため、グローバルに一意である保証がない。**
    -- ユーザーで名前空間を分けないと、他人のキーと衝突して
    -- 「他人の投稿結果が返る」という最悪の事故になりうる。
    --
    -- ON DELETE CASCADE は sessions に揃える。保持は 24 時間なので、
    -- 退会したユーザーのキーを残す理由がない。
    -- (ADR 0015 の DDL には無いが、sessions と同じ扱いにしている)
    user_id         BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key             TEXT        NOT NULL,

    -- どの経路で使われたキーか。診断用に残す。
    -- 一致判定そのものは request_hash が行う (endpoint も hash の入力に含める)。
    endpoint        TEXT        NOT NULL,

    -- 同じキーで別の内容を送られたことの検出用。
    --
    -- これが無いと、キーを使い回して別の内容を投稿しようとしたときに
    -- **黙って前回の結果が返る**。クライアントのバグが見えなくなる。
    request_hash    TEXT        NOT NULL,

    -- 記録した応答。再送に対してそのまま返す。
    -- 処理が完了するまで NULL のままになる。
    response_status INT,
    response_body   JSONB,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,

    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (user_id, key),

    -- **キーの長さを縛る。** ADR 0015 の DDL には無いが、
    -- 値がクライアント由来なので、上限が無いと巨大な文字列を
    -- そのまま保存させられる (ADR 0013 の防御の考え方に揃える)。
    -- 仕様書 (api/openapi.yaml の IdempotencyKey) と同じ上限にしてある。
    CONSTRAINT idempotency_keys_key_length CHECK (char_length(key) BETWEEN 1 AND 255)
);

-- 期限切れの削除 (保持 24 時間) が拾う行。
--
-- 古いキーを消さないと単調増加する。24 時間は
-- 「クライアントがリトライを諦めるまでの時間」として十分という判断。
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);

-- 外部キー用の索引は別に張らない。
-- 主キー (user_id, key) の先頭が user_id なので、
-- users からの参照整合性の確認にそのまま使える
-- (docs/adr/0016-schema-and-indexes.md の「外部キーには必ず索引を貼る」)。
