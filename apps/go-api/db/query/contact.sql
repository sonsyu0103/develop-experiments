-- =============================================================================
-- 問い合わせ (docs/adr/0008-contact-and-mail.md)
--
-- 【このファイルは 2 種類のクエリに割れている】
--   受付側  リクエストの中で走る。CreateContactMessage と CountRecentContacts
--   送信側  定期処理の中で走る。Claim... 以降
--
-- 分かれているのが決定 1 (保存と送信を分ける) そのものになる。
-- 受付側は外部サービスに触らないので、メールプロバイダが落ちていても
-- 問い合わせは受け取れる。
-- =============================================================================

-- name: CreateContactMessage :one
-- 問い合わせを 1 件保存する。**この時点ではまだ送っていない。**
--
-- status は既定の 'pending'、next_attempt_at は既定の now() に任せる。
-- 明示的に渡さないのは、**「受け付けたらすぐ送信対象になる」が既定である
-- ことをスキーマ側に持たせる**ため。ここで時刻を計算すると、
-- アプリの時計と DB の時計のずれが送信の遅延として出る。
--
-- **id を返すが、外には出さない。** ログ (contact_id) と
-- 送信側の追跡に使う内部の識別子で、応答には含めない
-- (ADR 0003 未決 #11 / OpenAPI の ContactAccepted を参照)。
INSERT INTO contact_messages (user_id, name, email, subject, body, client_ip)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, created_at;

-- name: CountRecentContacts :one
-- 同じ IP からの直近の件数を数える (ADR 0008 決定 4 のレート制限)。
--
-- 【なぜ専用のカウンタストアを置かないか】
-- **アプリのメモリで数えると、複数インスタンスでは効かない。**
-- ECS のタスクが N 個あれば実質 N 倍まで通る。閲覧数バッファ (ADR 0006) と
-- 同じ構造の問題だが、閲覧数は多少ずれてよいのに対し
-- **レート制限はずれると意味を失う**。
--
-- 問い合わせは 1 日数十件を想定しているので、
-- **contact_messages をそのまま数えれば足りる。**
-- contact_rate_limit_idx (client_ip, created_at) で引く。
--
-- **この方法が成立するのは問い合わせだからで、コメント投稿には使えない。**
-- 毎リクエストで集計クエリを走らせることになる。その段階になったら
-- WAF のレート制限ルールか Redis での集計を使う。
--
-- **client_ip が NULL の行は数に入らない。** 等値比較は NULL に一致しない
-- ため、消し込み済みの古い行は自動的に対象外になる。
-- 窓 (既定 1 時間) より古い行しか消し込まないので、実害は無い。
SELECT count(*) FROM contact_messages
WHERE client_ip = sqlc.arg('client_ip')
  AND created_at > now() - sqlc.arg('window')::interval;

-- name: ClaimPendingContacts :many
-- 送信する行を確保する。**このリポジトリで唯一の SKIP LOCKED。**
--
-- 【二重送信を防ぐ】
-- ワーカーは API プロセスの中で動くので (ADR 0003 未決 #9)、
-- **レプリカの数だけ同時に走る**。素朴に SELECT すると、
-- 同じ問い合わせが 2 通届く。
--
-- `FOR UPDATE SKIP LOCKED` は、他のトランザクションがロック中の行を
-- **待たずに飛ばす**。各インスタンスは互いに異なる行を取得する。
--   SKIP LOCKED 無し → 解放を待ち、待った先で処理済みの行を読む
--   NOWAIT          → エラーになる (どちらも使えない)
--
-- 【SELECT ではなく UPDATE にしている】
-- ADR 0008 の図は「取り出す → 送信 → status 更新」を 1 つの流れで書いているが、
-- そのまま 1 トランザクションにすると **SMTP の往復のあいだ行ロックを
-- 保持し続ける**。外部サービスの応答時間だけ長いトランザクションが残り、
-- プールの接続も 1 本占有する。
--
-- そこで**確保だけを 1 文で終わらせて即コミットする**。
-- 画像の回収 (ADR 0007 / image usecase の Reclaim) が
-- 「確保 → S3 削除 → DB 反映」と 3 段階に割っているのと同じ形になる。
--
-- 確保の中身は 2 つ:
--   attempt_count を先に増やす  クラッシュを繰り返す行が無限に居座らない
--   next_attempt_at を先送りする 送信中の行を他のインスタンスが拾わない
--                               (= リース。送信が落ちてもリース切れで戻る)
--
-- **これは at-least-once であって exactly-once ではない** (ADR 0008)。
-- 送信の直後、status を書く前にプロセスが落ちると、リース切れの後に
-- もう一度送られる。SMTP に冪等キーが無い以上ここは避けられないので、
-- **重複を許して取りこぼしを許さない**側を選んでいる。
-- 問い合わせが届かないほうが、2 通届くより悪い。
--
-- 【ORDER BY を索引と揃える】
-- contact_pending_idx が (next_attempt_at, id) WHERE status = 'pending' なので、
-- 同じ並びにすると索引をそのまま辿れる。id を第 2 キーに置くのは
-- next_attempt_at が一意でないため。
UPDATE contact_messages
SET attempt_count   = attempt_count + 1,
    next_attempt_at = now() + sqlc.arg('lease')::interval
WHERE id IN (
    SELECT id FROM contact_messages
    WHERE status = 'pending'
      AND next_attempt_at <= now()
    ORDER BY next_attempt_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT sqlc.arg('batch_size')
)
RETURNING id, user_id, name, email, subject, body, attempt_count, created_at;

-- name: MarkContactSent :execrows
-- 送信できた行を確定する。
--
-- **status = 'pending' を条件に含める。** 含めないと、リース切れで
-- 2 つのワーカーが同じ行を送ったときに、後から来たほうが
-- sent_at を上書きして「いつ送ったか」がずれる。
--
-- last_error を消すのは、前回の失敗理由が残っていると
-- 「送信済みなのにエラーが出ている行」に見えるため。
--
-- CHECK 制約 contact_sent_complete が status と sent_at の対応を要求する。
UPDATE contact_messages
SET status     = 'sent',
    sent_at    = now(),
    last_error = NULL
WHERE id = sqlc.arg('id')
  AND status = 'pending';

-- name: RescheduleContact :execrows
-- 送信に失敗した行を、次の試行へ回す。
--
-- **attempt_count はここで増やさない** —— 確保 (ClaimPendingContacts) が
-- 既に増やしている。両方で増やすと、1 回の失敗で 2 回ぶん減る。
--
-- next_attempt_at はリース (確保時に置いた仮の時刻) を、
-- 指数バックオフで計算し直した時刻で上書きする。
-- 待ち時間を DB に持つので、**プロセスが落ちても待ちは失われない**
-- (Phase 2 のプロセス内リトライとの違い)。
UPDATE contact_messages
SET next_attempt_at = now() + sqlc.arg('backoff')::interval,
    last_error      = sqlc.arg('last_error')
WHERE id = sqlc.arg('id')
  AND status = 'pending';

-- name: FailContact :execrows
-- 試行回数の上限を超えた行を打ち切る (ADR 0008 決定 1)。
--
-- **無限にリトライしない。** 送信できないメールが延々とプロバイダを
-- 叩き続けることになり、まともなメールの送達にも響く。
--
-- 行は消さない。「受け付けたが送れなかった問い合わせ」が残らないと、
-- 運用が手で拾い直すこともできなくなる。
UPDATE contact_messages
SET status     = 'failed',
    last_error = sqlc.arg('last_error')
WHERE id = sqlc.arg('id')
  AND status = 'pending';

-- name: OldestPendingContact :one
-- 最も古い未送信の問い合わせの受付時刻。**メトリクス用。**
--
-- ADR 0008 の「引き受けるコスト」が挙げているのがこれになる ——
-- ワーカーが止まっていることに気づく仕組みが無いと、
-- **問い合わせが静かに溜まり続ける**。送信が回っていれば
-- この値は常に数分以内で、止まると単調に古くなる。
--
-- 0 行なら未送信が無い (= 正常)。
--
-- **created_at で並べる。** contact_pending_idx は
-- (next_attempt_at, id) なので並べ替えが要るが、pending の集合は
-- 送信が追いついていれば常に小さい。止まっているときは大きくなるが、
-- そのときは既に検知したい状態そのものなので、遅くて構わない。
SELECT created_at FROM contact_messages
WHERE status = 'pending'
ORDER BY created_at
LIMIT 1;

-- name: ScrubContactClientIPs :execrows
-- 古い行から client_ip を消す。
--
-- **個人データを無期限に持たない** (ADR 0008「client_ip は個人データに
-- あたるため、無期限には保持しない」)。保持期間はログ (ADR 0010 決定 6 の
-- 400 日) と揃える —— 同じ IP がログ側にも出ているので、
-- 片方だけ短くしても消えたことにならない。
--
-- **行は消さない。** 消すと問い合わせの記録そのものが消える。
-- 消すのは列の値だけで、レート制限は窓 (既定 1 時間) の中でしか
-- 数えないため、古い行の値が無くても影響しない。
--
-- 一度に更新する件数を制限しているのは、期限切れキーの削除と同じ理由。
-- 呼び出し側は「0 行になるまで繰り返す」形で使う。
--
-- contact_ip_scrub_idx (created_at) WHERE client_ip IS NOT NULL で引く。
-- 述語に client_ip IS NOT NULL が入っているので、
-- **消し込みが進むほど索引が小さくなる。**
UPDATE contact_messages
SET client_ip = NULL
WHERE id IN (
    SELECT id FROM contact_messages
    WHERE client_ip IS NOT NULL
      AND created_at <= now() - sqlc.arg('retention')::interval
    ORDER BY created_at
    LIMIT sqlc.arg('max_rows')
);
