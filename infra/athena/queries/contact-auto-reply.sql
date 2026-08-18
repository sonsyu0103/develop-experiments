-- 控え (自動返信) が届いているか (docs/adr/0008-contact-and-mail.md 決定 2)。
--
-- **控えの成否は、ログにしか残らない。**
-- 運営への通知は contact_messages の status / sent_at / last_error に残るが、
-- 控えは best-effort として設計されており、**行に痕跡を 1 つも残さない。**
-- 「届かなかった」ことを知る手段はこのクエリだけになる。
--
-- =============================================================================
-- 見方: 失敗の件数より、まず sent が出ているかを見る
-- =============================================================================
-- ADR 0008 の「実装して分かったこと 9」と同じ構造の問題がある ——
-- **検知したい状態そのものが、検知の手段を止める。**
--
--   送信ワーカーが止まる → 控えも送られない → **このイベントが 1 件も出ない**
--
-- 「failed が 0 件」は「全部届いた」とも「1 通も試していない」とも読める。
-- そのため failed だけでなく **sent の件数と最終時刻**を並べてある。
-- 監視側では「値が閾値を超えたこと」ではなく、
-- **「一定時間 sent が出ていないこと」**をアラートの条件に入れること。
--
-- =============================================================================
-- 失敗が続いてよい場合がある
-- =============================================================================
-- SES のサンドボックスでは、**検証済みアドレス以外に送れない。**
-- 控えの宛先は利用者ごとに変わるので、解除申請が通るまでは全滅する。
-- そのとき failed は「送信経路の障害」ではなく「解除がまだ」を意味する ——
-- error の中身 (下の failure_reasons) を見ずに件数だけで判断しないこと。
--
-- =============================================================================
-- contact_id が NULL になっていたら DDL を疑う
-- =============================================================================
-- Phase 8 で contact_* を出し始めたとき、**table.sql に contact 系の列を
-- 1 つも足していなかった。** Athena はスキーマオンリードなので
-- エラーにならず静かに NULL になる (table.sql の罠 2)。
-- このクエリを書いたときに気づいて足した。
--
-- Athena で使うときは dt の絞り込みを足す。

SELECT
    date(from_iso8601_timestamp(time))          AS day,
    -- **sent を先に置く。** 0 なら「届いていない」ではなく
    -- 「1 通も試していない」を先に疑う (上記)。
    count_if(msg = 'contact_auto_reply_sent')   AS sent,
    count_if(msg = 'contact_auto_reply_failed') AS failed,
    -- **結線の誤り。** 宛先が空のまま送ろうとした (ADR 0008 決定 2)。
    -- 0 以外なら実装の問題で、送信経路とは無関係。
    count_if(msg = 'contact_auto_reply_address_empty') AS address_empty,
    -- users.email が形式不正で、控えを送る相手を作れなかった。
    -- IdP が返した値がそのまま入る列なので、0 でないこと自体はありうる。
    count_if(msg = 'contact_verified_email_invalid')   AS invalid_address,
    -- **最終時刻。** 監視はここを見る ——
    -- 「一定時間 sent が出ていない」がワーカー停止の合図になる。
    max(if(msg = 'contact_auto_reply_sent', time))     AS last_sent_at,
    -- 代表例を 1 件だけ。全文を並べると読めなくなる (errors.sql と同じ)。
    -- **宛先が混じることがある** —— RCPT TO の応答文にアドレスが載る
    -- (ADR 0008「引き受けるコスト」)。個人データとして扱うこと。
    any_value(if(msg = 'contact_auto_reply_failed', error)) AS example_error
FROM go_api_logs
WHERE msg IN (
    'contact_auto_reply_sent',
    'contact_auto_reply_failed',
    'contact_auto_reply_address_empty',
    'contact_verified_email_invalid'
)
GROUP BY date(from_iso8601_timestamp(time))
ORDER BY day DESC;
