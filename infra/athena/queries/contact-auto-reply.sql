-- 控え (自動返信) が届いているか (docs/adr/0008-contact-and-mail.md 決定 2)。
--
-- **手元 (DuckDB) で実行できる形で書いてある** (http-status.sql と同じ約束)。
-- Athena に持っていくときは dt の絞り込みを足す。無いと全期間をスキャンする。
--
-- 初版は Trino 専用の関数 (from_iso8601_timestamp / 2 引数の if) で書いており、
-- **DuckDB でも Athena でも一度も実行されないまま**だった (レビュー指摘)。
--
-- **日付は substr で切り出している。** time は RFC3339 なので先頭 10 文字が
-- そのまま YYYY-MM-DD になる。CAST(time AS TIMESTAMP) は DuckDB では通るが、
-- **Trino では T 区切りと末尾の Z の扱いが変わる** ——
-- 手元で確かめられないものを本番の唯一の集計手段に置かない。
-- 文字列比較でも ISO の日付は時系列順に並ぶ。
--
-- =============================================================================
-- 見方: failed の件数ではなく、received と sent の差を見る
-- =============================================================================
-- ADR 0008 の「実装して分かったこと 9」と同じ構造の問題がある ——
-- **検知したい状態そのものが、検知の手段を止める。**
--
--   送信ワーカーが止まる → 控えも送られない → **失敗すら 1 件も出ない**
--
-- 「failed が 0 件」は「全部届いた」とも「1 通も試していない」とも読める。
--
-- **だから contact_received を分母に入れてある** (レビュー指摘)。
-- これが無いと、問い合わせが 1 件も来なかった日 (この規模では平常) と
-- ワーカーが止まった日が同じ見え方になり、
-- 「一定時間 sent が出ていない」で組んだアラートが鳴りっぱなしになる。
--
-- 見るべきは received > 0 なのに sent も failed も出ていない日。
-- **控えは匿名の問い合わせには送らない**ので、
-- received と sent が一致しないこと自体は正常になる。
--
-- 滞留のほう (contact_pending_age の oldest_age_ms) も併せて見ると、
-- 「運営宛も止まっている」のか「控えだけ出ていない」のかが分かれる。
--
-- =============================================================================
-- 失敗が続いてよい場合がある
-- =============================================================================
-- SES のサンドボックスでは、**検証済みアドレス以外に送れない。**
-- 控えの宛先は利用者ごとに変わるので、解除申請が通るまでは全滅する。
-- そのとき failed は「送信経路の障害」ではなく「解除がまだ」を意味する ——
-- error の中身を見ずに件数だけで判断しないこと。
--
-- =============================================================================
-- contact_id が NULL になっていたら DDL を疑う
-- =============================================================================
-- Phase 8 で contact_* を出し始めたとき、**table.sql に contact 系の列を
-- 1 つも足していなかった。** Athena はスキーマオンリードなので
-- エラーにならず静かに NULL になる (table.sql の罠 2)。
-- このクエリを書いたときに気づいて足し、**同じ漏れが起きないよう
-- verify-log-events.py が DDL との差分を CI で見るようにした。**

SELECT
    substr(time, 1, 10)                                       AS day,
    -- **分母。** 控えの成否は、これと並べて初めて意味を持つ (上記)。
    count(*) FILTER (WHERE msg = 'contact_received')          AS received,
    count(*) FILTER (WHERE msg = 'contact_auto_reply_sent')   AS sent,
    count(*) FILTER (WHERE msg = 'contact_auto_reply_failed') AS failed,
    -- **結線の誤り。** 宛先が空のまま送ろうとした (ADR 0008 決定 2)。
    -- 0 以外なら実装の問題で、送信経路とは無関係。
    count(*) FILTER (WHERE msg = 'contact_auto_reply_address_empty') AS address_empty,
    -- users.email が形式不正で、控えを送る相手を作れなかった。
    -- IdP が返した値がそのまま入る列なので、0 でないこと自体はありうる。
    count(*) FILTER (WHERE msg = 'contact_verified_email_invalid')   AS invalid_address,
    -- **最終時刻。** received が 0 でない日にこれが古いままなら、
    -- 控えの経路が止まっている。
    max(CASE WHEN msg = 'contact_auto_reply_sent' THEN time END)     AS last_sent_at,
    -- 代表例を 1 件だけ。全文を並べると読めなくなる (errors.sql と同じ)。
    -- **宛先が混じることがある** —— RCPT TO の応答文にアドレスが載る
    -- (ADR 0008「引き受けるコスト」)。個人データとして扱うこと。
    any_value(CASE WHEN msg = 'contact_auto_reply_failed' THEN error END) AS example_error
FROM go_api_logs
WHERE msg IN (
    'contact_received',
    'contact_auto_reply_sent',
    'contact_auto_reply_failed',
    'contact_auto_reply_address_empty',
    'contact_verified_email_invalid'
)
GROUP BY substr(time, 1, 10)
ORDER BY day DESC;
