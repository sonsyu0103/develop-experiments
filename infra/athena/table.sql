-- Athena のテーブル定義 (docs/adr/0010-log-pipeline.md 決定 2)。
--
-- **このファイルは本番用で、手元では実行しない。** ローカルの検証は
-- DuckDB が読む (infra/duckdb/)。両者の差はモジュール冒頭に書いてある。
--
-- <bucket> を実際のバケット名に置き換えて実行する。
--
-- =============================================================================
-- 罠 1: service をパーティション列にはできない
-- =============================================================================
-- S3 のキーは logs/service=go-api/dt=.../hour=.../ で、Hive 形式としては
-- service もパーティションに見える。しかし**レコードの中にも service がある**
-- (ADR 0010 の 4-2 の共通フィールド)。
--
-- Athena はパーティション列とデータ列の同名を許さないため、
-- 両方を宣言した DDL は作成時に落ちる。
--
-- ここでは **service を LOCATION に埋め込み、パーティションにしない**。
-- ADR 0010 決定 2 の storage.location.template がもともとその形になっている。
--
--   LOCATION 's3://<bucket>/logs/service=go-api/'
--
-- 【サービスが増えたとき】
-- service をパーティションにしたくなるが、そのときは
-- **レコード側のフィールド名を変える**しかない (例: service → svc)。
-- ログのフィールド名は実質的な契約なので (ADR 0010「引き受けるコスト」)、
-- 過去データと新データでクエリを分ける覚悟が要る。
-- 別テーブルを service ごとに作るほうが、たいてい安い。
--
-- =============================================================================
-- 罠 2: 宣言しなかった列は「無い」ことになる
-- =============================================================================
-- Athena はスキーマオンリード。**エラーにならず、静かに NULL になる。**
-- アプリが新しいフィールドを出し始めても、ここに足すまで見えない。
-- 4-6 の業務イベントを増やしたら、この DDL も一緒に更新する。

CREATE EXTERNAL TABLE IF NOT EXISTS go_api_logs (
    -- 共通フィールド (4-2)。service は上の罠 1 のため宣言しない。
    `time`       string,
    `level`      string,
    `msg`        string,
    `version`    string,
    `request_id` string,
    `user_id`    bigint,

    -- http_request
    `method`     string,
    `path`       string,
    `status`     int,
    -- **ナノ秒ではなく ms** (4-4)。単位を名前に含めてある。
    `latency_ms` double,

    -- 業務イベント (4-6)
    `thread_id`  bigint,
    `comment_id` bigint,
    `seq`        int,
    -- コメント投稿が何回目の試行で成功したか (ADR 0019 決定 4)。
    -- **本番で直列化失敗を数える起点はここ。** serialization_failure は
    -- DEBUG なので本番の S3 には 1 件も届かない。
    `attempts`   int,
    -- 直列化失敗 (40001) と採番の衝突 (23505) は同じイベント名で出るので、
    -- モードごとの競合の現れ方はこれで見分ける (ADR 0019 決定 4)。
    `sqlstate`   string,
    -- ssi / pessimistic / unique / naive。Phase 4 でモード別に集計する。
    `mode`       string,
    -- 冪等キーで再生された応答か (ADR 0015)。
    `idempotent` boolean,
    -- 閲覧数のフラッシュ (ADR 0006)
    `threads`    bigint,
    `increments` bigint,

    -- 定期処理 (scheduler)
    `job`        string,
    `elapsed_ms` bigint,
    `rounds`     int,

    -- 起動時と防御層
    `addr`            string,
    `bucket`          string,
    `bootstrap_admin` boolean,
    -- csrf_rejected が「なぜ弾いたか」(ADR 0013)。
    -- **value に入るのは Origin ヘッダの値**で、利用者の入力ではない。
    `reason`          string,
    `value`           string,

    `error`      string,

    -- FireLens が付けるコンテナのメタデータ。
    -- Reserve_Data On で残している (これを落とすと、
    -- どのタスクが出したログか分からなくなる)。
    `container_id`   string,
    `container_name` string,
    `ecs_task_arn`   string,
    `source`         string,

    -- **JSON でない行はここに残る。** go run のコンパイルエラーや
    -- panic のスタックトレースはパースできず、log キーのまま素通しする。
    -- 障害時に最も見たい行なので、捨てずに拾えるようにしておく。
    `log`        string
)
PARTITIONED BY (
    `dt`   string,
    `hour` string
)
ROW FORMAT SERDE 'org.openx.data.jsonserde.JsonSerDe'
WITH SERDEPROPERTIES (
    -- パースできないレコードで**クエリ全体を落とさない**。
    -- ログ基盤は「壊れた 1 行のせいで何も読めない」が最も困る。
    'ignore.malformed.json' = 'true',
    'dots.in.keys'          = 'true'
)
LOCATION 's3://<bucket>/logs/service=go-api/'
TBLPROPERTIES (
    -- partition projection (決定 2)。
    --
    -- **これを外すと Athena は破産する。** パーティションが無いクエリは
    -- 毎回全期間を読む。加えて projection なら MSCK REPAIR TABLE も
    -- Glue Crawler も要らず、「昨日のログだけ検索できない」という
    -- Crawler 失敗の事故モードが丸ごと消える。
    'projection.enabled'        = 'true',
    'projection.dt.type'        = 'date',
    'projection.dt.format'      = 'yyyy-MM-dd',
    -- 開始日は運用開始日に合わせる。NOW は毎日自動で伸びる。
    'projection.dt.range'       = '2026-08-01,NOW',
    'projection.hour.type'      = 'integer',
    'projection.hour.range'     = '0,23',
    -- **2 桁に固定する。** s3_key_format の %H が 08 のような形なので、
    -- digits を指定しないと 8 で探しに行って 0 件になる。
    'projection.hour.digits'    = '2',
    'storage.location.template' = 's3://<bucket>/logs/service=go-api/dt=${dt}/hour=${hour}/'
);
