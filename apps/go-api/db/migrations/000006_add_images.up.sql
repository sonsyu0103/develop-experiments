-- =============================================================================
-- 000006: 画像 (images) と、添付先 3 つへの参照列
-- =============================================================================
-- 判断の記録は docs/adr/0007-image-storage.md と
-- docs/adr/0016-schema-and-indexes.md (索引とマイグレーション表)。
--
-- 画像を投稿できるのはログイン済みユーザーだけ (ADR 0007 の背景)。
-- 匿名で任意のバイト列をストレージに置けると、容量の消費と
-- 違法コンテンツの設置が追跡不能な形で可能になる。
--
-- **DB を先に書き、後から確定させる** (ADR 0007 決定 3)。
--   1. images に status = 'pending' で INSERT   ← ここで object_key を決める
--   2. ストレージへ PUT
--   3. images を status = 'committed' に UPDATE
--   4. 添付先から参照させる
-- 2 か 3 で落ちると 'pending' の行が残る。これは**追跡できる孤児**であり、
-- 回収バッチが拾える。逆順にするとストレージ側に記録の無いオブジェクトが残り、
-- 全件リストと突き合わせないと見つけられない。
-- =============================================================================

CREATE TABLE images (
    -- UUID v7 を Go 側 (google/uuid) で採番する。
    -- PostgreSQL 17 には uuidv7() が無い (18 で追加)。
    --
    -- 連番にしない理由は「キーが推測できると他人の画像を辿れる」
    -- (docs/adr/0003-open-questions.md 未決 #11)。
    -- v4 ではなく v7 なのは、B-tree の挿入位置が末尾に寄るため。
    id           UUID        PRIMARY KEY,

    -- 所有者。「書いた人」ではなく「置いた人」なので author_id ではなく owner_id
    -- (ADR 0016 の命名の統一)。投稿を消しても所有者は変わらない。
    --
    -- **ON DELETE を付けない (= RESTRICT)。**
    -- sessions / idempotency_keys は CASCADE にしているが、あちらは
    -- 消えても失われるものが無い。画像は実体が S3 にあるため、
    -- DB 行だけ消えると**回収できない孤児オブジェクト**になる。
    -- そもそも退会は論理削除 (users.deleted_at) なので、この経路は通らない。
    owner_id     BIGINT      NOT NULL REFERENCES users(id),

    -- 用途。添付先のテーブルを兼ねて表す。
    kind         TEXT        NOT NULL,

    -- ストレージ上のキー。**利用者の入力を使わない** (ADR 0007 決定 3)。
    -- ファイル名をそのままキーにすると、パストラバーサル・
    -- 他人のオブジェクトの上書き・キーの衝突がすべて可能になる。
    object_key   TEXT        NOT NULL UNIQUE,

    -- 再エンコード後の形式。**こちらが生成した値**であり、利用者の申告ではない。
    content_type TEXT        NOT NULL,

    width        INT         NOT NULL,
    height       INT         NOT NULL,
    byte_size    BIGINT      NOT NULL,

    -- 'pending' | 'committed' | 'deleted'
    --
    -- 'deleted' はモデレーターが消した画像 (ADR 0011 決定 5)。
    -- **行は残す** —— コメントやスレッドから参照されているため、
    -- 消すと外部キー違反になり、かつ「画像は削除されました」と
    -- 「元から画像なし」を区別できなくなる (ADR 0016 問題 3)。
    status       TEXT        NOT NULL,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    committed_at TIMESTAMPTZ,

    CONSTRAINT images_status_valid CHECK (status IN ('pending', 'committed', 'deleted')),
    CONSTRAINT images_kind_valid   CHECK (kind IN ('comment_attachment', 'avatar', 'thread_icon')),

    -- 以下 3 つは ADR 0007 の DDL には無い。既存テーブルの流儀
    -- (comments_body_length などの「最後の砦」) に揃えて足す。
    -- 通常の検証はアプリ側で済ませており、ここはバグを止めるためだけにある。

    -- 寸法は必ず正。0 や負数は再エンコードの実装が壊れたときにしか入らない。
    CONSTRAINT images_dimensions_positive CHECK (width > 0 AND height > 0),

    -- サイズも必ず正。0 バイトは「PUT したつもりで何も書けていない」状態。
    CONSTRAINT images_byte_size_positive CHECK (byte_size > 0),

    -- **出力する形式だけを許す** (ADR 0007 決定 6)。
    -- content_type はこちらが生成する値なので、ここに利用者の申告が
    -- 入っていたらアプリのバグになる。形式を増やすときはマイグレーションが要る
    -- —— それは意図した変更として残すべきものになる。
    CONSTRAINT images_content_type_valid CHECK (content_type IN ('image/jpeg', 'image/webp')),

    -- committed_at と status の整合。
    -- 'pending' のまま committed_at が入る / 'committed' なのに入っていない、
    -- はどちらも決定 3 の手順が壊れたことを意味する。
    CONSTRAINT images_committed_at_matches_status CHECK (
        (status = 'pending'  AND committed_at IS NULL) OR
        (status <> 'pending' AND committed_at IS NOT NULL)
    )
);

-- 回収バッチが拾う行 (ADR 0007 決定 3 / ADR 0016 問題 3)。
--
-- 放置された 'pending' と、モデレーターが消した 'deleted' の両方を対象にする。
-- 扱いは status で分かれる:
--   pending の孤児  → DB 行も S3 オブジェクトも消す
--   deleted        → S3 オブジェクトだけ消す。DB 行は残す
--
-- 部分インデックスにしているのは、大多数を占める 'committed' を
-- 索引に載せないため。回収バッチはこの述語でしか引かない。
CREATE INDEX images_reclaimable_idx
    ON images (created_at)
    WHERE status IN ('pending', 'deleted');

-- マイページの「自分が上げた画像」用 (ADR 0016 では区分 C)。
--
-- **区分 C だが貼る。** 先頭列が owner_id なので、
-- users からの参照整合性の確認にそのまま使えるため
-- (「外部キーには必ず索引を貼る」= 区分 A)。
-- idempotency_keys が主キー (user_id, key) で兼ねているのと同じ形になる。
CREATE INDEX images_owner_created_idx ON images (owner_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- 添付先 3 つ
-- ---------------------------------------------------------------------------
--
-- どれも NULL 許容。NULL が「画像なし」を意味する。
-- PostgreSQL 11 以降、NULL 許容の ADD COLUMN はテーブルを書き換えない。
--
-- **users と images は相互に参照する** (images.owner_id -> users.id,
-- users.avatar_image_id -> images.id)。循環に見えるが、どちらも
-- 「先に相手を作ってから参照を張る」順 (画像を作る -> 利用者に紐付ける) で
-- 進むため、挿入順で解ける。両方が NOT NULL だったら解けなかった。

-- コメント 1 件に画像 1 枚 (ADR 0007)。
-- 複数枚にすると中間テーブルが要り、パーティション済みテーブルとの
-- 結合がもう 1 段増える。必要になってから拡張する。
--
-- comments はハッシュパーティション済みだが、親への ADD COLUMN は
-- 8 パーティションすべてに伝播する (ADR 0007 のスキーマ節)。
ALTER TABLE comments ADD COLUMN image_id UUID REFERENCES images(id);
ALTER TABLE users    ADD COLUMN avatar_image_id UUID REFERENCES images(id);
ALTER TABLE threads  ADD COLUMN icon_image_id UUID REFERENCES images(id);

-- ---------------------------------------------------------------------------
-- 外部キー用の索引
-- ---------------------------------------------------------------------------
--
-- **PostgreSQL は外部キーを張っても、参照する側に索引を自動作成しない**
-- (ADR 0016)。参照先の行が削除されるとき参照元を走査するため、
-- 索引が無いと全表走査になる。回収バッチは images の行を消すので、
-- ここを落とすと画像 1 枚の削除がテーブル全体の走査になる。
--
-- **述語は「何を除外するか」で選ぶ。** deleted_at IS NULL で絞ると
-- WHERE image_id = $1 から条件を導けず、索引が候補にすら入らない。
-- IS NOT NULL なら strict な演算子から導ける (ADR 0016)。
-- 画像を付けない投稿が多数派になるので、索引も小さくなる。

-- ここが最も重要。comments は 8 パーティション・将来的に億単位の行を持つ。
CREATE INDEX comments_image_id_idx ON comments (image_id) WHERE image_id IS NOT NULL;

-- **ADR 0016 の索引一覧に、この 1 本だけ載っていない。**
-- comments.image_id と threads.icon_image_id は表にあるのに、
-- users.avatar_image_id が抜けている (ADR 0007 は列の存在を書いている)。
-- 同 ADR 自身の「外部キーには必ず索引を貼る」に照らして貼る。
CREATE INDEX users_avatar_image_id_idx ON users (avatar_image_id) WHERE avatar_image_id IS NOT NULL;

CREATE INDEX threads_icon_image_id_idx ON threads (icon_image_id) WHERE icon_image_id IS NOT NULL;
