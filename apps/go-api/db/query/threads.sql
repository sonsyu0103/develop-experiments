-- name: ListThreadsWithCommentCount :many
-- スレッド一覧 + コメント数を「1 往復」で取得する。
--
-- 【なぜ LEFT JOIN + GROUP BY にしないか】
-- 素直に書くと以下になるが、これは 20 件返すために
-- 20 スレッド分のコメント実データ (数千行) をヒープから読んでしまう。
--
--   SELECT t.*, COUNT(c.id) FILTER (WHERE c.deleted_at IS NULL)
--   FROM threads t LEFT JOIN comments c ON c.thread_id = t.id
--   GROUP BY t.id ORDER BY t.id DESC LIMIT 20
--
-- 先に LIMIT でスレッドを 20 件へ絞り、その各行に対して
-- 相関サブクエリで数えると、部分インデックス
-- comments_alive_thread_id_desc_idx だけで完結しヒープにほぼ触れない。
--
-- 【実測 1: 投稿者の LEFT JOIN を足す前】
-- 2,005 スレッド / 200,016 コメント (EXPLAIN ANALYZE、各 3 回):
--
--   LEFT JOIN + GROUP BY : 5.84 - 7.43 ms / shared buffers 2,054
--   相関サブクエリ       : 1.04 - 1.11 ms / shared buffers    59
--
-- 約 5.5 倍速く、バッファ読み取りは 35 分の 1。
--
-- 【実測 2: 投稿者の LEFT JOIN を含む現在の形】(Phase 4 / make query-probe)
-- 2,000 スレッド / 201,000 コメント / 200 利用者:
--
--   LEFT JOIN + GROUP BY : 4.13 ms / shared buffers 7,205
--   相関サブクエリ       : 1.71 ms / shared buffers 1,664
--
-- **約 2.4 倍速く、バッファは 4.3 分の 1。差は縮んだ。**
-- 両者に共通のコスト (users の解決) が加わったぶん、相対差が縮む。
--
-- **2 つの数字を直接比べないこと。** コメントの分布が違う
-- (実測 2 のデータセットはスレッドごとに 1〜200 件と散らしてある)。
-- 言えるのは「相関サブクエリのほうが速い」が保たれていることまでになる。
--
-- 結果が一致することは確認済み (両者を FULL JOIN して差分 0 件)。
-- どちらも DB への往復は 1 回なので、N+1 実装との対比は変わらない。
--
-- 【planner は部分索引ではなく主キーを選んだ】
-- この規模・この分布では、`ORDER BY id DESC LIMIT 20` に
-- threads_alive_id_desc_idx ではなく **threads_pkey の逆順走査**が選ばれる。
-- 削除済みが 1% しかないため、主キーを逆に辿って 20 件を取るほうが安い、
-- という判断になる。
--
-- **部分索引が無駄だったわけではない。** 深いページ (カーソルつき) では
-- threads_alive_id_desc_idx が選ばれる (make query-probe の 3 番)。
-- 論理削除の比率が上がれば先頭ページでも選ばれるようになる。
--
-- 【注意】この差は Index Only Scan が効くことに依存する。
-- バルク INSERT 直後は visibility map が未整備で Heap Fetches が発生し、
-- 一時的に旧実装と同程度まで劣化する。VACUUM (autovacuum を含む) の後に
-- Heap Fetches: 0 となって本来の性能が出る。
-- ベンチマーク (Phase 4) では VACUUM 後に計測すること。
--
-- ページネーションは OFFSET ではなくキーセット (cursor) 方式。
-- OFFSET は「読み飛ばす行を実際に読む」ため、深いページほど線形に遅くなる。
--
-- 【投稿者は LEFT JOIN で解決する】
-- ADR 0014 の選択肢 A。コメント数の集約とは事情が違い、
-- users.id は主キーなので 1:1 の参照になる。
--
-- **LEFT であることが必須。** INNER にすると author_id IS NULL の
-- 匿名投稿が一覧から丸ごと消える (ADR 0005 決定 2)。
--
-- JOIN は page で 20 件に絞ったあとに掛ける。
-- 内側の CTE に混ぜると、絞り込む前の全行に対して結合が走る。
WITH page AS (
    SELECT id, title, created_at, view_count, author_id, icon_image_id
    FROM threads
    WHERE deleted_at IS NULL
      AND (sqlc.narg('cursor_id')::bigint IS NULL OR id < sqlc.narg('cursor_id')::bigint)
    ORDER BY id DESC
    LIMIT sqlc.arg('page_size')
)
SELECT
    p.id,
    p.title,
    p.created_at,
    p.view_count,
    (
        SELECT count(*)
        FROM comments c
        WHERE c.thread_id = p.id
          AND c.deleted_at IS NULL
    )::bigint AS comment_count,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at,
    img.id         AS icon_id,
    img.object_key AS icon_object_key,
    img.width      AS icon_width,
    img.height     AS icon_height
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
LEFT JOIN images img ON img.id = p.icon_image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL
ORDER BY p.id DESC;

-- name: ListPopularThreadsWithCommentCount :many
-- スレッド一覧を閲覧数の多い順に取得する (Phase 7 / ADR 0006)。
--
-- 【なぜ ListThreadsWithCommentCount と 1 本にまとめないか】
-- 並び順が違うとカーソルの比較そのものが変わる。
-- 新着順は `id < $1` の 1 列比較、人気順は `(view_count, id) < ($1, $2)` の
-- 行値比較で、条件をパラメータで切り替えると
-- **どちらの実行でも索引を選びきれない形**になる
-- (SearchThreadsWithCommentCount を分けたのと同じ理由)。
--
-- 【行値比較を使う】
-- AND/OR に展開した条件と違い、これは索引をそのまま辿れる。
--
--   -- 展開した形 (索引を辿れない)
--   WHERE view_count < $1 OR (view_count = $1 AND id < $2)
--
-- **id を第 2 キーに入れるのが必須。** 閲覧数が同値のスレッドは必ず存在し、
-- 同値の並びが不定だとページ境界で行が重複・欠落する。
--
-- 【カーソルの OR はここにも残っている】
-- 先頭ページを NULL で表す形は新着順から引き継いだもの。
-- 3 本とも同じ形を保つ意味があるので、直すなら 3 本同時になる
-- (SearchThreadsWithCommentCount の同じ節を参照)。
--
-- 【並びは「閲覧数が動かない」ことに依存している】
-- 閲覧数を毎リクエスト更新していたら、ページ送りの最中に view_count が動き、
-- 行の重複と取りこぼしが起きる。**フラッシュ間隔の間は動かない**ので、
-- 数百 ms で終わるページ送りは安定した並びを辿れる。
-- これは ADR 0006 で D (バッファリング) を選んだことの副産物になる。
WITH page AS (
    SELECT id, title, created_at, view_count, author_id, icon_image_id
    FROM threads
    WHERE deleted_at IS NULL
      AND (
        sqlc.narg('cursor_view_count')::bigint IS NULL
        OR (view_count, id) < (sqlc.narg('cursor_view_count')::bigint, sqlc.narg('cursor_id')::bigint)
      )
    ORDER BY view_count DESC, id DESC
    LIMIT sqlc.arg('page_size')
)
SELECT
    p.id,
    p.title,
    p.created_at,
    p.view_count,
    (
        SELECT count(*)
        FROM comments c
        WHERE c.thread_id = p.id
          AND c.deleted_at IS NULL
    )::bigint AS comment_count,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at,
    img.id         AS icon_id,
    img.object_key AS icon_object_key,
    img.width      AS icon_width,
    img.height     AS icon_height
FROM page p
LEFT JOIN users u ON u.id = p.author_id
LEFT JOIN images img ON img.id = p.icon_image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL
ORDER BY p.view_count DESC, p.id DESC;

-- name: IncrementThreadViewCounts :execrows
-- バッファに溜まった閲覧数をまとめて反映する (Phase 7 / ADR 0006)。
--
-- **呼び出し側は必ず別トランザクション・READ COMMITTED で実行すること。**
-- コメント投稿・スレッド作成のトランザクションに混ぜてはいけない。
-- 混ぜると threads への rw-conflict が大量に生まれ、
-- 閲覧という無関係な操作が Phase 2 の直列化失敗率を押し上げる。
--
-- 【なぜ 1 文でまとめるか】
-- 1 行ずつ UPDATE すると、フラッシュ 1 回で数百往復になる。
-- DB への往復回数をフラッシュ間隔で決めることが D の目的なので、
-- そこを往復数で失っては意味がない。
--
-- 【ロックを id 昇順で取る】
-- 複数のインスタンスが**異なる順序で同じ行集合を更新するとデッドロックする。**
-- 各インスタンスは自分のバッファに溜まった集合を反映するので、
-- 集合は重なるが順序は揃わない。
--
-- **UPDATE ... FROM の行処理順は保証されない。** 実行計画次第で、
-- 入力配列の順序どおりにロックを取るとは限らない。
-- そのため、先に FOR UPDATE の CTE で **ORDER BY id のロックだけを取る。**
-- FOR UPDATE を含む CTE は inline されず、UPDATE 本体より先に評価される。
--
-- 【存在しない行は黙って無視される】
-- 閲覧された後に削除されたスレッドは JOIN で落ちる。
-- 削除済みへの加算を弾く必要はない —— 一覧にも詳細にも出ないため、
-- 値が残っていても表に出ない。復活の機能を入れるなら、そのとき考える。
--
-- 【なぜ unnest を 2 つ並べないか】
-- unnest(ids, increments) と 2 引数で書くのが素直だが、
-- **sqlc の解析器が「function unnest(unknown, unknown) does not exist」で
-- 生成に失敗する** (PostgreSQL は通る)。
-- 1 引数の unnest に WITH ORDINALITY を付け、
-- もう一方の配列を添字で引く形にすると生成できる。
WITH input AS (
    SELECT
        t.id,
        (sqlc.arg('increments')::bigint[])[t.ord] AS increment
    FROM unnest(sqlc.arg('thread_ids')::bigint[]) WITH ORDINALITY AS t(id, ord)
), locked AS (
    SELECT t.id
    FROM threads t
    JOIN input i ON i.id = t.id
    ORDER BY t.id
    FOR UPDATE OF t
)
UPDATE threads t
SET view_count = t.view_count + i.increment
FROM input i
WHERE t.id = i.id
  AND t.id IN (SELECT id FROM locked);

-- name: SearchThreadsWithCommentCount :many
-- タイトルの中間一致でスレッドを絞り込む (Phase 11 / ADR 0012)。
--
-- 【なぜ ListThreadsWithCommentCount と 1 本にまとめないか】
-- 絞り込みの有無をパラメータで分ける書き方だと、こうなる。
--
--   WHERE deleted_at IS NULL
--     AND (sqlc.narg('q')::text IS NULL OR title ILIKE '%' || ... || '%')
--
-- **この OR は索引を殺しうる。** 検索語が渡っている実行では
-- 「NULL かどうか」の分岐が定数に畳まれてほしいが、それが起きるのは
-- パラメータ値を見て計画を立てたとき (custom plan) だけになる。
-- PostgreSQL は同じ prepared statement を繰り返し実行すると
-- 汎用計画 (generic plan) に切り替えることがあり、そうなると
-- OR の左辺を畳めず、GIN 索引を使えないまま全表走査に落ちる。
--
-- **「たまに遅い」がいちばん困る形**なので、クエリを 2 本に割って
-- 計画を分ける。SELECT 句の重複は承知のうえで、
-- 索引が使われるかどうかを実行のたびに賭けないほうを採る。
--
-- 【では下の cursor_id の OR はなぜ残っているか】(レビュー指摘)
-- **同じ OR でも、失うものが違う。**
--
--   検索語の OR    畳めないと GIN 索引を**一切使えない**。
--                  珍しい語で全表走査に落ちる (ADR 0012 の実測で 26.75 ms)
--   カーソルの OR  畳めなくても索引は使える。**開始位置**が
--                  「カーソルの行」から「先頭」に落ちるだけで、
--                  1 ページ目の性能は変わらない
--
-- 加えて、この OR は ListThreadsWithCommentCount から引き継いだもので、
-- **直すなら 3 本を同時に直す話**になる (ページ送りの検査もやり直す)。
--
-- 【Phase 4 で測った結果: 置き換えない】(make query-probe)
-- 2,000 スレッド、カーソルを最も深い位置に置いた実測 (各 3 回の中央値):
--
--   IS NULL OR (現行) : 0.11 ms / buffers 167 / threads_alive_id_desc_idx
--   COALESCE (番兵)   : 0.11 ms / buffers 167 / threads_alive_id_desc_idx
--
-- **差が無い。** どちらも同じ索引を同じように辿る。
-- 先頭ページ (カーソルなし) でも 0.10 - 0.11 ms で並ぶ。
--
-- 番兵の値 (9223372036854775807) を SQL に持ち込む代償のほうが大きいので、
-- **現行の OR を維持する。** 上に書いたとおり、カーソルの OR は
-- 畳めなくても索引は使える —— 失うのは開始位置だけで、
-- 検索語の OR とは違って索引そのものを失わない。
--
-- 【なぜ ILIKE か】
-- pg_trgm の索引はトライグラムを小文字化して持つため、
-- ILIKE でも索引が効く。ロケールは C だが、日本語には大小の区別がなく
-- 実質 LIKE と同じ挙動になる。英数字のタイトルだけが恩恵を受ける。
--
-- 【ワイルドカードは呼び出し側でエスケープ済み】
-- title_query には % _ \ をエスケープした文字列が入る。
-- 生のまま渡すと、利用者が '%' の 1 文字で全件一致を作れる (ADR 0012 の罠)。
-- エスケープは永続化層の仕事にしてある —— LIKE の構文は
-- PostgreSQL の都合であって、ドメインが知るべきことではない。
--
-- 【並び順は新着順のまま】
-- 関連度順にしない理由は ADR 0012 決定 3。id DESC のままなら
-- キーセットページネーションがそのまま使える。
--
-- 索引の選び方は planner に任せている。検索語が珍しいほど
-- GIN (threads_title_trgm_idx) が有利で、ありふれた語ほど
-- id 順に読んで捨てる threads_alive_id_desc_idx が有利になる。
WITH page AS (
    SELECT id, title, created_at, view_count, author_id, icon_image_id
    FROM threads
    WHERE deleted_at IS NULL
      AND title ILIKE '%' || sqlc.arg('title_query')::text || '%' ESCAPE '\'
      AND (sqlc.narg('cursor_id')::bigint IS NULL OR id < sqlc.narg('cursor_id')::bigint)
    ORDER BY id DESC
    LIMIT sqlc.arg('page_size')
)
SELECT
    p.id,
    p.title,
    p.created_at,
    p.view_count,
    (
        SELECT count(*)
        FROM comments c
        WHERE c.thread_id = p.id
          AND c.deleted_at IS NULL
    )::bigint AS comment_count,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at,
    img.id         AS icon_id,
    img.object_key AS icon_object_key,
    img.width      AS icon_width,
    img.height     AS icon_height
FROM page p
LEFT JOIN users u ON u.id = p.author_id
LEFT JOIN images img ON img.id = p.icon_image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL
ORDER BY p.id DESC;

-- name: GetThreadWithCommentCount :one
-- 一覧と同じ理由で、JOIN + GROUP BY ではなく相関サブクエリで数える。
SELECT
    t.id,
    t.title,
    t.created_at,
    t.view_count,
    (
        SELECT count(*)
        FROM comments c
        WHERE c.thread_id = t.id
          AND c.deleted_at IS NULL
    )::bigint AS comment_count,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at,
    img.id         AS icon_id,
    img.object_key AS icon_object_key,
    img.width      AS icon_width,
    img.height     AS icon_height
FROM threads t
LEFT JOIN users u ON u.id = t.author_id
LEFT JOIN images img ON img.id = t.icon_image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL
WHERE t.id = sqlc.arg('id')
  AND t.deleted_at IS NULL;

-- name: CreateThread :one
-- RETURNING により INSERT と採番値の取得が 1 往復で完結する。
-- MySQL では LAST_INSERT_ID() を別クエリで叩く必要がある。
--
-- 【なぜ CTE で LEFT JOIN まで済ませるか】
-- RETURNING は挿入した行しか返せず、users を結合できない。
-- 投稿直後のレスポンスにも投稿者を載せる必要があるため、
-- ここで引かないと「作成時だけ author が null」という不整合になる。
-- 呼び出し側で組み立てる手もあるが、一覧・詳細と組み立て方が 2 通りになる。
--
-- author_id は NULL 許容。NULL が匿名を意味する (ADR 0005 決定 2)。
WITH inserted AS (
    INSERT INTO threads (title, author_id, icon_image_id)
    VALUES (sqlc.arg('title'), sqlc.narg('author_id'), sqlc.narg('icon_image_id'))
    RETURNING id, title, created_at, view_count, author_id, icon_image_id
), attached AS (
    -- アイコンを「添付済み」にする (000007)。理由は comments.sql と同じ。
    -- スレッドのアイコンは作成時にしか指定できないので、解除の経路は無い。
    UPDATE images
    SET attached_at = now()
    WHERE id = (SELECT icon_image_id FROM inserted)
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
    i.title,
    i.created_at,
    i.view_count,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at,
    img.id         AS icon_id,
    img.object_key AS icon_object_key,
    img.width      AS icon_width,
    img.height     AS icon_height
FROM inserted i
LEFT JOIN users u ON u.id = i.author_id
LEFT JOIN images img ON img.id = i.icon_image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL;

-- name: ThreadExists :one
SELECT EXISTS (
    SELECT 1 FROM threads
    WHERE id = sqlc.arg('id') AND deleted_at IS NULL
);

-- name: SoftDeleteOwnThread :execrows
-- 投稿者が自分のスレッドを論理削除する (ADR 0005 の権限モデル / ADR 0003 未決 #7)。
--
-- **消えるのは「生きている自分のスレッド」だけ。** 3 つの条件が揃わないと
-- 0 行になる。0 行だった理由 (無い / 他人のもの / 匿名) は
-- ThreadOwnership が別に答える。
--
-- 【匿名投稿は当たらない】
-- author_id が NULL のとき `author_id = $2` は NULL (真ではない) になるので、
-- **明示的な IS NOT NULL は要らない。** 三値論理がそのまま
-- 「本人であることを示せない」を表している。
-- 匿名投稿を消せるのはモデレーターだけ (ADR 0011 決定 2)。
--
-- 【コメントも添付画像も消さない】
-- スレッドが論理削除されると一覧・取得のどちらからも消えるため、
-- コメントを個別に消して回る必要がない (8 パーティションすべてに
-- UPDATE を投げることにもなる)。画像を実際に消すのはモデレーションの操作
-- (ADR 0011 決定 5) で、自分の投稿を消しただけで実体まで消すと
-- 誤操作の巻き戻しができなくなる。
UPDATE threads
SET deleted_at = now()
WHERE id = sqlc.arg('id')
  AND author_id = sqlc.arg('actor_id')
  AND deleted_at IS NULL;

-- name: ThreadOwnership :one
-- 削除が 0 行だったときに、その理由を答える。
--
-- **成功したときには引かない。** 呼ぶのは失敗の分類のためだけで、
-- 常に 2 往復させるためではない。
--
-- 行が返らなければ「無い、または既に消えている」= 404。
-- 返って owned = false なら「他人のもの、または匿名」= 403。
--
-- 【1 文にまとめなかった理由】
-- 判定と更新を CTE 1 本に畳む形も書けるが、**sqlc の解析器が
-- CTE と更新対象テーブルのスコープを混ぜてしまい、
-- author_id を ambiguous として生成に失敗する** (PostgreSQL は通る)。
-- 生成器に通らない形を無理に維持するより、失敗経路でだけ
-- もう 1 往復するほうが読みやすい。
--
-- 分けたことで「消せなかった理由」の判定には競合の余地が残るが、
-- **削除そのものは 1 文で閉じている**ので、二重削除や
-- 他人の投稿が消えることは起きない。ずれても
-- 403 と 404 を取り違えるだけになる。
-- **::boolean が要る。** 付けないと sqlc が型を推論できず、
-- 生成される戻り値が interface{} になる (実測)。
SELECT COALESCE(author_id = sqlc.arg('actor_id'), false)::boolean AS owned
FROM threads
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- name: SoftDeleteThread :execrows
-- スレッドを論理削除する (ADR 0011 決定 2)。
--
-- **deleted_at IS NULL を条件に含める。** 含めないと 2 回目以降も
-- 1 行を返し、呼び出し側が「今回消した」と判断してしまう。
-- モデレーション記録は「実際に起きた変化」に対してだけ書きたい。
-- 副次的に、消した時刻が後から呼ばれた削除で上書きされるのも防ぐ。
--
-- **コメントは消さない。** スレッドが論理削除されると一覧・取得の
-- どちらからも消えるため、コメントを個別に消して回る必要がない。
-- 8 パーティションすべてに UPDATE を投げることにもなる。
--
-- **添付画像も消さない。** アイコンやコメントの画像を
-- ストレージから消すかはモデレーターの別の判断であり
-- (スレッドの主題が不適切でも画像は問題ないことがある)、
-- delete_image として個別に記録されるべきものになる。
UPDATE threads
SET deleted_at = now()
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- -----------------------------------------------------------------------------
-- 以下 3 つは Phase 4 のベンチマーク専用 (N+1 実装の再現用)。
-- 本番経路では使わない。
--
-- 【この 3 つで「同じ仕事」を組み立てられること】
-- 単一クエリ版 (ListThreadsWithCommentCount) は 1 往復で
-- 「スレッド + コメント数 + 投稿者」を返す。N+1 版が返す情報が
-- それより少ないと、**同じ仕事をしていない 2 つを比べる**ことになる
-- (ADR 0014「測定の前提が 1 つ崩れている」)。
-- 3 つを合わせて単一クエリ版と同じ列が揃うようにしてある。
-- -----------------------------------------------------------------------------

-- name: ListThreadsOnly :many
-- コメント数も投稿者も引かずにスレッドだけを取得する。
--
-- **view_count まで引く。** 単一クエリ版の内側 CTE と同じ列を読む形にして、
-- ヒープから取り出す幅を揃える。1 列足りないだけで
-- 「N+1 側のほうが読む量が少ない」比較になる。
--
-- **旧称は ListThreadIDs。** id しか引いていなかった頃の名前が
-- 列を足したあとも残っていた。リポジトリ側のメソッド名 (ListThreadsOnly) に揃える。
SELECT id, title, created_at, view_count
FROM threads
WHERE deleted_at IS NULL
  AND (sqlc.narg('cursor_id')::bigint IS NULL OR id < sqlc.narg('cursor_id')::bigint)
ORDER BY id DESC
LIMIT sqlc.arg('page_size');

-- name: CountCommentsByThreadID :one
SELECT COUNT(*)
FROM comments
WHERE thread_id = sqlc.arg('thread_id')
  AND deleted_at IS NULL;

-- name: FindThreadAuthor :one
-- 1 スレッド分の投稿者を引く (ADR 0014 の選択肢 C「素朴に 1 件ずつ引く」)。
--
-- **threads を経由して引く。** users.id を直接受け取る形にすると、
-- 内部 ID がユースケース層まで出てくる。ADR 0014 は
-- 「内部 ID は運ばない」と決めているので、ベンチマーク用の経路でも破らない。
-- 往復回数は変わらないため、測りたいものは変わらない。
--
-- **匿名投稿では 0 行になる** (JOIN が空振りする)。
-- 呼び出し側は「行が無い = 匿名」として扱う。
--
-- 単一クエリ版の LEFT JOIN users と同じ列を返す。
-- 退会済みも返し、表示の差し替えはドメイン (model.NewAuthor) が行う。
SELECT u.public_id, u.display_name, u.avatar_url, u.deleted_at
FROM threads t
JOIN users u ON u.id = t.author_id
WHERE t.id = sqlc.arg('thread_id');
