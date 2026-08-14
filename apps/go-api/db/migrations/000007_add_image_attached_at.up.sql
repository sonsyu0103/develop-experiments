-- =============================================================================
-- 000007: images.attached_at と、回収バッチの部分索引の張り直し
-- =============================================================================
-- 000006 で作った images_reclaimable_idx は述語が
--
--     WHERE status IN ('pending', 'deleted') AND object_reclaimed_at IS NULL
--
-- だった。回収の対象が「放置された pending」と「モデレーターが消した deleted」
-- の 2 種類だけだった頃の述語になる。
--
-- **Phase 6 後半で 3 種類目 (committed の孤立) が増えた時点で破綻した。**
-- アップロードと添付を分けた結果 (ADR 0007 の実装して分かったこと 1)、
-- 「アップロードしたが投稿をやめた」画像が committed のまま誰からも
-- 参照されずに残る。これを拾うためクエリに
--
--     OR (status = 'committed' AND NOT EXISTS (...) x3)
--
-- を足したが、**この行は上の索引の述語に入らない**。プランナは
-- 「索引に載っていない行が対象に含まれる」クエリで部分索引を選べないため、
-- 10 分ごと・レプリカごとに images の全表走査 + created_at のソート +
-- 行ごとに 3 本の anti-join が走る形になっていた。
-- committed が行の大多数を占める設計なので、育つほど悪化する。
--
-- 【なぜ「参照されているか」を列で持つのか】
-- NOT EXISTS x3 は**索引の述語に書けない**。部分索引の述語は
-- その行だけで判定できる式に限られ、他テーブルを見る副問い合わせは使えない。
-- つまり「参照されていないこと」を images 自身の列に落とさない限り、
-- 索引で絞る方法が無い。
--
-- 【それでもクエリからは NOT EXISTS を消さない】
-- attached_at はアプリが書く値なので、**新しい添付経路を足した人が
-- 書き忘れると、参照されている画像を消しにいく**。
-- 索引で候補を数件に絞ったあと、消す直前に NOT EXISTS で確かめる形にする
-- (候補が少ないので、外部キー用の部分索引を引く安い検査になる)。
-- 列は速さのため、NOT EXISTS は正しさのため、と役割を分ける。
-- =============================================================================

-- 添付先から参照された時刻。NULL が「まだどこからも参照されていない」。
--
-- **書くのはアプリ**になる (添付と同じ 1 文の中で更新する)。
-- 経路は 3 つ + 解除が 1 つ:
--
--   comments.image_id       CreateComment / CreateCommentAutoSeq
--   threads.icon_image_id   CreateThread
--   users.avatar_image_id   SetUserAvatarImage      ← 付け替え時に旧画像を NULL へ戻す
--
-- 解除があるのはアバターだけ。コメントとスレッドは作成時にしか
-- 画像を指定できず、削除は論理削除なので参照は残る。
ALTER TABLE images ADD COLUMN attached_at TIMESTAMPTZ;

-- 既存行の埋め戻し。images は 000006 で作ったばかりだが、
-- **述語を変える前に現状と辻褄を合わせておく**。
-- ここを飛ばすと、既に添付済みの画像が「未添付」に見えて回収対象に入る
-- (最後は外部キーが拒否するが、その前に S3 の実体が消える)。
UPDATE images
SET attached_at = COALESCE(committed_at, created_at)
WHERE EXISTS (SELECT 1 FROM comments c WHERE c.image_id = images.id)
   OR EXISTS (SELECT 1 FROM users    u WHERE u.avatar_image_id = images.id)
   OR EXISTS (SELECT 1 FROM threads  t WHERE t.icon_image_id = images.id);

-- 最後の砦 (comments_body_length などと同じ位置づけ)。
-- pending は「ストレージへの書き込みが確定していない」状態なので、
-- そこから添付できたら決定 3 の手順が壊れている。
ALTER TABLE images ADD CONSTRAINT images_attached_at_requires_commit
    CHECK (attached_at IS NULL OR status <> 'pending');

-- 回収バッチが拾う行。**3 種類すべてがこの述語に収まる。**
--
--   pending の孤児    attached_at IS NULL (pending は添付できない)
--   committed の孤立  attached_at IS NULL
--   deleted           status = 'deleted' (添付済みでも S3 は消す)
--
-- deleted を OR で足しているのは、モデレーターが消した画像が
-- **添付されたまま**回収対象になるため。attached_at だけでは拾えない。
--
-- 索引に載るのは「まだ添付されていない画像」と「消されてまだ回収していない画像」
-- だけになる。どちらも通常は数分〜1 時間で抜けるので、索引は小さいまま保たれる。
-- 大多数を占める「添付済みの committed」は載らない ——
-- 000006 の部分索引が狙っていたものが、ここで初めて成立する。
--
-- **クエリ側は WHERE にこの式をそのまま書くこと。** 部分索引が使われるのは
-- 索引の述語がクエリの制約から導けるときだけで、OR を含む式は
-- 「同じ式が書かれている」形でしか一致しない。
DROP INDEX images_reclaimable_idx;

CREATE INDEX images_reclaimable_idx
    ON images (created_at)
    WHERE object_reclaimed_at IS NULL
      AND (attached_at IS NULL OR status = 'deleted');
