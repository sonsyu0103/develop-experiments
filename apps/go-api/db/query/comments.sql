-- name: ListCommentsByThreadID :many
-- thread_id を等値で指定しているため、HASH パーティションの pruning が効き、
-- 8 分割中 1 パーティションだけを走査する。
--
-- 【なぜスレッドの存在確認を同じクエリに含めないか】
-- 「存在しないスレッド」と「コメント 0 件のスレッド」を 1 クエリで
-- 区別するには、threads を駆動表にした LEFT JOIN LATERAL が必要になる。
-- しかしその形ではコメント側の列が NULL になりうるのに対し、
-- sqlc は LEFT JOIN の NULL 許容を推論できず int64 / string を生成する
-- (実測: コメント 0 件のスレッドで comment_id と author_name が NULL 返却)。
-- 結果として Scan が実行時に失敗する。
--
-- 往復 1 回を節約するために実行時エラーの危険を持ち込むのは割に合わないため、
-- 存在確認は呼び出し側 (CommentInteractor) の別クエリに分けている。
-- どちらも主キー / 部分インデックスで完結する軽いクエリである。
--
-- 【投稿者は LEFT JOIN で解決する (ADR 0014 の選択肢 A)】
-- **LEFT であることが必須。** INNER にすると匿名コメントが消える。
--
-- ADR 0014 は「コメント一覧では選択肢 B (一括取得) が勝つ可能性がある」と
-- 書いている。同じ人が連投すると、同じ投稿者の情報が最大 100 行ぶん
-- 重複して転送されるため。初手は A で揃え、B (ListAuthorsByIDs) との
-- 比較は Phase 4 のベンチマークで行う。
WITH page AS (
    SELECT id, thread_id, seq, author_name, body, created_at, author_id, image_id
    FROM comments
    WHERE thread_id = sqlc.arg('thread_id')
      AND deleted_at IS NULL
      AND (sqlc.narg('cursor_id')::bigint IS NULL OR id < sqlc.narg('cursor_id')::bigint)
    ORDER BY id DESC
    LIMIT sqlc.arg('page_size')
)
SELECT
    p.id,
    p.thread_id,
    p.seq,
    p.author_name,
    p.body,
    p.created_at,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at,
    -- 添付画像も LEFT JOIN で解決する。
    -- **LEFT であることが必須** (INNER にすると画像なしのコメントが消える)。
    -- 参照は部分索引 comments_image_id_idx を使う。
    i.id         AS image_id,
    i.object_key AS image_object_key,
    i.width      AS image_width,
    i.height     AS image_height
FROM page p
LEFT JOIN users u ON u.id = p.author_id
-- **実体が無い画像は結合しない** (レビュー指摘)。
--   status = 'deleted'          モデレーターが消した。回収バッチが S3 から実体を消す
--   object_reclaimed_at IS NOT NULL  回収済み。実体はもう無い
-- どちらも URL を返すと、ブラウザには壊れた画像が出る。
-- **条件は ON に置くこと。** WHERE に置くと LEFT が INNER に化けて、
-- 画像なしの行 (大多数) が消える。
--
-- ADR 0016 問題 3 は「画像は削除されました」と「元から画像なし」を
-- 区別するために行を残すと決めているが、**その区別を表す API の項目がまだ無い。**
-- Phase 10 後半で削除の UI を入れるとき、ここを status の受け渡しに変える。
LEFT JOIN images i ON i.id = p.image_id
    AND i.status <> 'deleted' AND i.object_reclaimed_at IS NULL
ORDER BY p.id DESC;

-- name: NextCommentSeq :one
-- スレッド内の次のレス番号を求める。**Phase 2 の題材の中心** (ADR 0019 決定 1)。
--
-- この 1 文は comments_thread_id_seq_idx の逆順スキャン 1 回で終わる。
-- 速いが、**速さは正しさと関係がない** —— 同時に実行した 2 つの
-- トランザクションは、どちらも同じ値を読む。
-- 読んだ値が有効であり続けることを保証するのは、呼び出し側の
-- 分離レベル (SERIALIZABLE) か明示ロック (LockThreadForUpdate) になる。
--
-- deleted_at で絞らないのは、削除されたコメントの番号を再利用しないため
-- (ADR 0019 決定 5)。再利用すると過去の >>5 が別の投稿を指すようになる。
--
-- ::int で明示的にキャストしているのは、COALESCE(MAX(...), 0) + 1 の
-- 型推論が sqlc 側で interface{} に落ちるのを避けるため。
SELECT (COALESCE(MAX(seq), 0) + 1)::int AS next_seq
FROM comments
WHERE thread_id = sqlc.arg('thread_id');

-- name: CreateCommentWithSeq :one
-- レス番号を呼び出し側が決めて挿入する。
-- ssi / pessimistic / naive の 3 モードが共有する (ADR 0019 決定 2)。
--
-- **親スレッドの存在確認をこの文に含めていない。**
-- 3 モードはいずれもトランザクションの中で、先に threads を読んでいる
-- (SERIALIZABLE では述語ロック、悲観ロックでは FOR UPDATE)。
-- ここで WHERE EXISTS を重ねると、
--   - SERIALIZABLE では同じ読み取りを 2 回行うだけ
--   - 「0 行が返る」原因が「スレッドが無い」と「採番が衝突した」の
--     2 通りになり、呼び出し側でエラーを取り違える
-- 存在確認をどこでやるかは、モードごとに呼び出し側が持つ。
--
-- 投稿者の解決は 1 往復に含める。RETURNING は挿入行しか返せないので
-- CTE で包んで LEFT JOIN する (threads.sql の CreateThread と同じ理由)。
WITH inserted AS (
    INSERT INTO comments (thread_id, seq, author_name, body, author_id, image_id)
    VALUES (
        sqlc.arg('thread_id'),
        sqlc.arg('seq'),
        sqlc.arg('author_name'),
        sqlc.arg('body'),
        sqlc.narg('author_id'),
        sqlc.narg('image_id')
    )
    RETURNING id, thread_id, seq, author_name, body, created_at, author_id, image_id
), attached AS (
    -- **画像を「添付済み」にするのは、投稿を作るのと同じ 1 文の中で行う** (000007)。
    -- 分けると「コメントは作られたが添付の記録が無い」窓ができ、
    -- そこに回収バッチが入ると参照中の画像の実体を消してしまう。
    --
    -- inserted を参照しているので、挿入が 0 行なら何も更新しない。
    -- こちらは VALUES なので必ず 1 行入るが、下の CreateCommentAutoSeq と
    -- 形を揃えておく (片方だけ別の書き方だと、直すときに見落とす)。
    UPDATE images
    SET attached_at = now()
    WHERE id = (SELECT image_id FROM inserted)
      AND attached_at IS NULL
      -- **確保済みの画像は添付済みにしない** (レビュー指摘)。
      -- EnsureOwned はロックを取らない読み取りなので、
      -- 「確認したあと・書く前」に回収バッチが確保する窓がある。
      -- この UPDATE は images の行ロックで待たされてから最新版を読むため、
      -- ここに条件を置くと確保を追い越せない。
      AND object_reclaimed_at IS NULL
)
SELECT
    i.id,
    i.thread_id,
    i.seq,
    i.author_name,
    i.body,
    i.created_at,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at,
    img.id         AS image_id,
    img.object_key AS image_object_key,
    img.width      AS image_width,
    img.height     AS image_height
FROM inserted i
LEFT JOIN users u ON u.id = i.author_id
LEFT JOIN images img ON img.id = i.image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL;

-- name: CreateCommentAutoSeq :one
-- 採番と挿入を 1 文で行う。unique モード専用 (ADR 0019 決定 2)。
--
-- **1 文にしても競合は消えない。** 集約はスナップショットから計算されるため、
-- 同時に走った 2 つの文は同じ MAX(seq) を読む。
-- 違うのは「衝突したことが必ず一意制約違反 (23505) として返る」点であり、
-- 呼び出し側がそれをリトライすることで正しさが保たれる。
-- READ COMMITTED のまま 1 往復で済むので、実務ではこれが最も安い。
--
-- 【HAVING であって WHERE ではない】
-- 親スレッドの生存確認を WHERE に置くと壊れる。
-- WHERE は集約の**入力行**を絞るため、スレッドが削除済みでも
-- 入力 0 行の集約が 1 行 (MAX = NULL) を返し、seq = 1 で挿入されてしまう。
-- HAVING は集約後の 1 行を絞るので、意図どおり 0 行になる。
-- 0 行のときは pgx.ErrNoRows として 404 に翻訳される。
--
-- 【23505 の制約名は子パーティションのもの】
-- パーティション親に張った comments_thread_id_seq_idx への違反は、
-- 実際には子の索引で検出されるため、SQLSTATE 23505 が返すのは
-- **comments_p5_thread_id_seq_idx のような子の名前**になる (実測)。
-- 親の名前だけで一致を見るとリトライ判定が永久に偽になり、
-- unique モードが競合のたびに 409 を返すようになる。
WITH inserted AS (
    INSERT INTO comments (thread_id, seq, author_name, body, author_id, image_id)
    SELECT
        sqlc.arg('thread_id'),
        COALESCE(MAX(c.seq), 0) + 1,
        sqlc.arg('author_name'),
        sqlc.arg('body'),
        sqlc.narg('author_id'),
        sqlc.narg('image_id')
    FROM comments c
    WHERE c.thread_id = sqlc.arg('thread_id')
    HAVING EXISTS (
        SELECT 1 FROM threads
        WHERE id = sqlc.arg('thread_id') AND deleted_at IS NULL
    )
    RETURNING id, thread_id, seq, author_name, body, created_at, author_id, image_id
), attached AS (
    -- **画像を「添付済み」にするのは、投稿を作るのと同じ 1 文の中で行う** (000007)。
    -- 分けると「コメントは作られたが添付の記録が無い」窓ができ、
    -- そこに回収バッチが入ると参照中の画像の実体を消してしまう。
    --
    -- **inserted を参照しているので、挿入が 0 行なら何も更新しない。**
    -- CreateCommentAutoSeq は親スレッドが消えていると 0 行になる ——
    -- そこで画像を添付済みにすると、投稿されていないのに
    -- 二度と回収されない孤児が残る。
    UPDATE images
    SET attached_at = now()
    WHERE id = (SELECT image_id FROM inserted)
      AND attached_at IS NULL
      -- **確保済みの画像は添付済みにしない** (レビュー指摘)。
      -- EnsureOwned はロックを取らない読み取りなので、
      -- 「確認したあと・書く前」に回収バッチが確保する窓がある。
      -- この UPDATE は images の行ロックで待たされてから最新版を読むため、
      -- ここに条件を置くと確保を追い越せない。
      AND object_reclaimed_at IS NULL
)
SELECT
    i.id,
    i.thread_id,
    i.seq,
    i.author_name,
    i.body,
    i.created_at,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at,
    img.id         AS image_id,
    img.object_key AS image_object_key,
    img.width      AS image_width,
    img.height     AS image_height
FROM inserted i
LEFT JOIN users u ON u.id = i.author_id
LEFT JOIN images img ON img.id = i.image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL;

-- name: SoftDeleteOwnComment :execrows
-- 投稿者が自分のコメントを論理削除する (ADR 0005 の権限モデル / ADR 0003 未決 #7)。
--
-- 条件と、0 行だった理由を別に引く事情は
-- threads.sql の SoftDeleteOwnThread と同じ。
--
-- 【thread_id が要る】
-- **主キーが (thread_id, id) なので、先頭列が無いと 8 パーティション
-- すべてを走査する。** 副次的に「他スレッドのコメント ID を渡しても
-- 当たらない」が成立する。
--
-- 【レス番号は消さない】
-- 行が残るので seq も残り、次の投稿は削除された番号の次から続く
-- (ADR 0019 決定 5)。再利用すると過去の >>5 が別の投稿を指す。
UPDATE comments
SET deleted_at = now()
WHERE thread_id = sqlc.arg('thread_id')
  AND id = sqlc.arg('id')
  AND author_id = sqlc.arg('actor_id')
  AND deleted_at IS NULL;

-- name: CommentOwnership :one
-- 削除が 0 行だったときに、その理由を答える。用途は ThreadOwnership と同じ。
-- **::boolean が要る。** 付けないと sqlc が型を推論できず、
-- 生成される戻り値が interface{} になる (実測)。
SELECT COALESCE(author_id = sqlc.arg('actor_id'), false)::boolean AS owned
FROM comments
WHERE thread_id = sqlc.arg('thread_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- name: SoftDeleteComment :execrows
-- **投稿者を見ない削除。** モデレーターの経路
-- (POST /moderation/actions) がこれを使う ——
-- 匿名投稿も消せる必要があるため (ADR 0011 決定 2)。
--
-- 本人による削除は SoftDeleteOwnComment のほう。
-- ADR 0003 の未決 #7 は Phase 10 後半で解決済み。
UPDATE comments
SET deleted_at = now()
WHERE thread_id = sqlc.arg('thread_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- name: LockThreadForUpdate :one
-- スレッド行に行ロックを取る。**pessimistic モードの起点** (ADR 0019 決定 2)。
--
-- 同じスレッドへの投稿をこの 1 行で直列化する。
-- レス番号の採番はこのロックを取ったあとに行うため、
-- 「読んだ MAX(seq) が他トランザクションに書き換えられる」ことが起きない。
--
-- 行が返らない場合は「スレッドが無い / 論理削除済み」であり、
-- 存在確認をこの 1 文が兼ねている。
--
-- **ロックの対象が threads であって comments でないことが重要。**
-- 採番は「まだ存在しない行」を巡る競合なので、コメント側の行ロックでは防げない
-- (ロックできる行が無い)。親を掴んで範囲ごと直列化する必要がある。
--
-- SSI (SERIALIZABLE) を使うならこの明示ロックは不要だが、
-- 悲観ロック版と楽観 (SSI) 版のスループット差を計測するために両方用意している。
SELECT id
FROM threads
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL
FOR UPDATE;

-- name: CommentExists :one
-- コメントが存在し、論理削除されていないかを返す。
--
-- **通報の対象を確かめるために要る** (ADR 0011 決定 4)。
-- 確かめずに積むと、存在しない ID の通報でキューを埋められる。
--
-- thread_id が要るのは主キーが (thread_id, id) だから ——
-- 無いと 8 パーティションすべてを走査する。
-- 通報のリクエストが threadId を受け取るのは、この検査のためでもある。
SELECT EXISTS (
    SELECT 1 FROM comments
    WHERE thread_id = sqlc.arg('thread_id')
      AND id = sqlc.arg('id')
      AND deleted_at IS NULL
);
