// Package config はアプリケーション起動時の設定を環境変数から読み取ります。
package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config はアプリケーション全体の設定です。
type Config struct {
	// Addr は HTTP サーバの待ち受けアドレスです。
	Addr string
	// DatabaseURL は PostgreSQL への接続文字列 (postgres://...) です。
	DatabaseURL string
	// MaxConns は接続プールの最大接続数です。
	MaxConns int32
	// MinConns はアイドル時に維持する最小接続数です。
	MinConns int32
	// ShutdownTimeout はグレースフルシャットダウンの猶予時間です。
	ShutdownTimeout time.Duration
	// AllowedOrigins は CORS で許可するオリジンの一覧です。
	AllowedOrigins []string
	// Debug は開発モードかどうかです。gin のモード切り替えに使います。
	//
	// ENV=development のときだけ true になります。
	// 未設定なら false (本番扱い) です。設定を忘れた環境が
	// 気づかないうちにデバッグモードで動くほうが危険なため、
	// 安全側に倒しています。
	Debug bool
	// LogFormat はログの出力形式です (docs/adr/0010-log-pipeline.md)。
	//
	// **Debug と分けてあります。** 初版はログ形式をデバッグモードに
	// 束ねており、ENV=development では必ずテキスト形式になっていました。
	// その結果、ログ基盤をローカルで検証すると
	// **fluent-bit のパーサが 1 行も展開できない** —— 本番だけ JSON、
	// 手元は logfmt なので、パイプラインの検証が本番の形に対して
	// 行われていませんでした (ADR 0010「実装して分かったこと 1」)。
	//
	// レベル (何を出すか) と形式 (どう出すか) は独立した関心なので、
	// 別々に決められるようにします。
	LogFormat LogFormat
	// CommentPostMode はコメント投稿の並行制御の方式です。
	CommentPostMode CommentPostMode
	// Auth は Google OIDC の設定です。
	Auth AuthConfig
	// Storage は画像を置くオブジェクトストレージの設定です。
	Storage StorageConfig
	// ViewCount は閲覧数の計上に関する設定です。
	ViewCount ViewCountConfig
}

// ViewCountConfig は閲覧数の計上に関する設定です
// (docs/adr/0006-view-count-and-popularity.md)。
//
// **無効化する設定はありません。** 閲覧数が増えていないことは
// 誰も検算しないので目に見えず、「無効なまま運用していた」に
// 気づく手立てが無いためです。調整できるのは頻度と窓だけになります。
type ViewCountConfig struct {
	// FlushInterval はバッファを DB へ反映する間隔です。
	//
	// **短くするほど値は新しくなり、UPDATE の回数が増えます。**
	// 閲覧数と無関係に、この間隔だけで更新頻度が決まるのが
	// バッファリングを選んだ理由そのものです。
	FlushInterval time.Duration
	// DedupeWindow は同一の訪問者を数え直さない時間です。
	//
	// Phase 4 で「抑制あり / なし」を比べられるよう、外から変えられます。
	DedupeWindow time.Duration
	// Mode は閲覧数の反映方式です。
	Mode ViewCountMode
}

// ViewCountMode は閲覧数の反映方式です
// (docs/adr/0006-view-count-and-popularity.md)。
type ViewCountMode string

const (
	// ViewCountModeBuffered はメモリに貯めてまとめて反映します。**既定。**
	ViewCountModeBuffered ViewCountMode = "buffered"
	// ViewCountModeSync は閲覧のたびに UPDATE を打ちます (選択肢 A)。
	//
	// **Phase 4 の比較専用で、本番相当の設定では選べません。**
	// これを本番で使うと、閲覧というまったく無関係な操作が
	// コメント投稿の直列化失敗率を押し上げます。
	// COMMENT_POST_MODE の naive と同じ扱いにしてあります。
	ViewCountModeSync ViewCountMode = "sync"
)

// viewCountModes は選べる方式と、本番相当の設定で許すかどうかです。
var viewCountModes = map[ViewCountMode]bool{
	ViewCountModeBuffered: true,
	ViewCountModeSync:     false,
}

// parseViewCountMode は VIEW_COUNT_MODE を読み取ります。
//
// 未知の値をエラーにする理由は parseCommentPostMode と同じです。
// 綴りを間違えたまま起動すると、ベンチマークで
// 「sync を測ったつもりの buffered の値」が出ます。
func parseViewCountMode(raw string, debug bool) (ViewCountMode, error) {
	if raw == "" {
		return ViewCountModeBuffered, nil
	}

	mode := ViewCountMode(strings.TrimSpace(strings.ToLower(raw)))
	allowedInProduction, known := viewCountModes[mode]
	if !known {
		return "", fmt.Errorf(
			"config: VIEW_COUNT_MODE が不正です (got %q, 選べるのは buffered / sync)", raw)
	}
	if !allowedInProduction && !debug {
		return "", fmt.Errorf(
			"config: VIEW_COUNT_MODE=%s は ENV=development でのみ選べます "+
				"(コメント投稿の直列化失敗率を押し上げます)", mode)
	}
	return mode, nil
}

// StorageConfig は S3 互換ストレージの設定です (docs/adr/0007-image-storage.md)。
//
// **すべて任意です。** AuthConfig と同じ理由で、揃っていない場合は
// 画像の経路だけが 503 になります。掲示板の閲覧・匿名投稿・
// ログインは画像に依存しません。
type StorageConfig struct {
	// Bucket はオブジェクトを置くバケット名です。
	Bucket string
	// Region は S3 のリージョンです。MinIO では任意の値で動きますが、
	// SDK が空を許さないため既定値を持たせます。
	Region string
	// Endpoint は S3 互換エンドポイントです。
	//
	// **本番 (実 S3) では空にします。** 空なら SDK がリージョンから
	// 正規のエンドポイントを組み立てます。MinIO のときだけ指定します。
	Endpoint string
	// UsePathStyle は path-style アクセスを使うかどうかです。
	//
	// MinIO では true が必要になります。既定の virtual-hosted style は
	// bucket.host という名前解決を要求し、localhost では引けません。
	UsePathStyle bool
	// AccessKeyID / SecretAccessKey は静的な資格情報です。
	//
	// **本番では空にします。** 空なら SDK の既定チェーン
	// (タスクロールなど) で解決させます —— キーを環境変数に置かない構成を
	// 選べるようにしておくためです。
	AccessKeyID     string
	SecretAccessKey string
	// PublicBaseURL は配信 URL の前置きです。
	//
	// API は絶対 URL を返すため (ADR 0007 決定 5)、環境ごとの差を
	// ここで吸収します。本番は CloudFront のドメイン、
	// ローカルは MinIO のエンドポイント + バケット名になります。
	PublicBaseURL string
}

// Enabled は画像を扱えるだけの設定が揃っているかを返します。
//
// **Endpoint と資格情報は判定に含めません。** どちらも
// 「本番では空にする」ことに意味がある項目なので、必須にすると
// 実 S3 の構成を選べなくなります。
func (s StorageConfig) Enabled() bool {
	return s.Bucket != "" && s.PublicBaseURL != ""
}

// CommentPostMode はコメント投稿の並行制御の方式です。
//
// 4 つの実装を同一バイナリのまま切り替えられるようにしてあります
// (docs/adr/0019-comment-concurrency.md 決定 2)。
// モードごとにビルドを分けると、Phase 4 の比較に
// 「ビルド差」という交絡が入ります。
type CommentPostMode string

const (
	// CommentPostModeSSI は SERIALIZABLE + 直列化失敗のリトライです。
	//
	// **このリポジトリの主題ですが、既定ではありません。**
	// 実測で 3 つの正しいモードの中で最も遅く、リトライも最も多かったため
	// (ADR 0019 の「実測」節)。
	CommentPostModeSSI CommentPostMode = "ssi"
	// CommentPostModePessimistic は threads の行ロック (FOR UPDATE) です。
	CommentPostModePessimistic CommentPostMode = "pessimistic"
	// CommentPostModeUnique は 1 文で採番し、一意制約違反をリトライします。
	// **既定値。** 実測で最も速く、往復も 1 回で済みます。
	CommentPostModeUnique CommentPostMode = "unique"
	// CommentPostModeNaive は防御なしの実装です。**レス番号が重複します。**
	//
	// 「SSI を入れたら正しくなった」は、入れる前が本当に壊れていたことを
	// 示さない限り主張になりません。それを実測するためだけに存在します。
	CommentPostModeNaive CommentPostMode = "naive"
)

// commentPostModes は選択できるモードと、本番相当の設定で許すかどうかです。
var commentPostModes = map[CommentPostMode]bool{
	CommentPostModeSSI:         true,
	CommentPostModePessimistic: true,
	CommentPostModeUnique:      true,
	// naive は ENV=development でしか選べません。
	CommentPostModeNaive: false,
}

// parseCommentPostMode は COMMENT_POST_MODE を読み取ります。
//
// **未知の値は既定値に落とさずエラーにします。** 綴りを間違えたときに
// 黙って ssi で動くと、ベンチマークで「pessimistic を測ったつもりの ssi の値」が
// 出ます。測定を汚す間違いは、起動時に止めるほうが安い。
func parseCommentPostMode(raw string, debug bool) (CommentPostMode, error) {
	if raw == "" {
		return CommentPostModeUnique, nil
	}

	mode := CommentPostMode(strings.TrimSpace(strings.ToLower(raw)))
	allowedInProduction, known := commentPostModes[mode]
	if !known {
		return "", fmt.Errorf(
			"config: COMMENT_POST_MODE が不正です (got %q, 選べるのは ssi / pessimistic / unique / naive)", raw)
	}
	if !allowedInProduction && !debug {
		// 既定を安全側に倒す方針は AuthConfig.SecureCookie と同じです。
		return "", fmt.Errorf(
			"config: COMMENT_POST_MODE=%s は ENV=development でのみ選べます (レス番号が重複します)", mode)
	}
	return mode, nil
}

// LogFormat はログの出力形式です。
type LogFormat string

const (
	// LogFormatJSON は 1 行 1 JSON です。**本番と、ログ基盤を通す経路の形式。**
	// Athena / DuckDB はこの形を前提にスキーマオンリードで読みます。
	LogFormatJSON LogFormat = "json"
	// LogFormatText は人間が読むための logfmt 形式です。
	// **fluent-bit のパーサはこれを展開できません** (JSON ではないため)。
	LogFormatText LogFormat = "text"
)

// logFormats は選べる形式です。
var logFormats = map[LogFormat]bool{
	LogFormatJSON: true,
	LogFormatText: true,
}

// parseLogFormat は LOG_FORMAT を読み取ります。
//
// 未設定のときは debug から決めます (開発はテキスト、それ以外は JSON)。
// **既定を残すのは、この変更で既存の環境の見え方を変えないためです。**
// ログ基盤を通す環境 (compose / 本番) は LOG_FORMAT=json を明示します。
//
// 未知の値をエラーにするのは parseCommentPostMode と同じ理由です。
// 綴りを間違えたまま黙ってテキストで動くと、S3 に着地したログが
// **1 行も展開されないまま貯まり続けます。** 気づくのは、Athena で
// クエリを書こうとした数日後になります。
// **空白だけは既定に落とさずエラーです** (parseCommentPostMode と同じ)。
// 空文字は compose の ${LOG_FORMAT:-} が普通に生む形なので未設定と同義に
// 扱いますが、空白だけが入るのは打ち間違いしかありません。
func parseLogFormat(raw string, debug bool) (LogFormat, error) {
	if raw == "" {
		if debug {
			return LogFormatText, nil
		}
		return LogFormatJSON, nil
	}

	format := LogFormat(strings.TrimSpace(strings.ToLower(raw)))
	if !logFormats[format] {
		return "", fmt.Errorf("config: LOG_FORMAT が不正です (got %q, 選べるのは json / text)", raw)
	}
	return format, nil
}

// AuthConfig は Google OIDC による認証の設定です。
//
// **すべて任意です。** 揃っていない場合、API は起動しますが
// **ログインの 2 経路だけ**が 503 を返します
// (`GET /auth/google` と そのコールバック)。
//
// 必須にしない理由:
//   - 掲示板の閲覧と匿名投稿は認証に依存しない。認証の設定が無いだけで
//     API 全体が起動しないのは害のほうが大きい
//   - CI の Migration Check は API サーバを起動してスモークテストを回す。
//     必須にすると、Google の資格情報を CI に置くまで CI が落ちる
//
// **この設定はセッションの検証を左右しません** (ADR 0005 決定 4)。
// 発行済みのセッションは、資格情報が無い環境でも解決されます ——
// そうしないと、CI で認証済みの経路を 1 件も検証できなくなります。
type AuthConfig struct {
	// GoogleClientID / GoogleClientSecret は Google Cloud で発行する資格情報です。
	GoogleClientID     string
	GoogleClientSecret string
	// RedirectURL は Google からのコールバック先です。
	// Google Cloud 側の「承認済みのリダイレクト URI」と一致している必要があります。
	//
	// **既定値を持たせません。** localhost を既定にすると、本番で
	// AUTH_REDIRECT_URL を入れ忘れても Enabled() が true になり、
	// Google に redirect_uri=http://localhost:8080/... を送って
	// redirect_uri_mismatch で初めて気づくことになります。
	// 未設定なら認証ごと無効 (503) にするほうが、原因が分かりやすくなります。
	RedirectURL string
	// FrontendURL はログイン完了後に戻す先です。RedirectURL と同じ理由で
	// 既定値を持たせません。
	FrontendURL string
	// BootstrapAdminGoogleSub は最初の管理者にする Google の sub です。
	//
	// **UI から管理者を作れないようにするための設定**です
	// (docs/adr/0011-moderation.md 決定 1)。
	// 「最初の 1 人」を作る機能は、そのまま
	// 「誰でも管理者になれる」機能になりえます。
	//
	// 未設定なら誰も昇格しません。認証の有効・無効とは独立です
	// (Enabled() の判定には含めません)。
	BootstrapAdminGoogleSub string
	// SecureCookie は Cookie に Secure 属性を付けるかどうかです。
	//
	// localhost は HTTP なので開発時は付けられません。
	// ENV=development 以外では既定で true になります —— 設定を忘れた本番が
	// Secure なしで動くほうが危険なため、安全側に倒しています。
	SecureCookie bool
}

// Enabled は認証を有効にできるだけの設定が揃っているかを返します。
func (a AuthConfig) Enabled() bool {
	return a.GoogleClientID != "" && a.GoogleClientSecret != "" &&
		a.RedirectURL != "" && a.FrontendURL != ""
}

// Load は環境変数から設定を読み取ります。
// 必須の環境変数が欠けている場合はエラーを返します。
func Load() (*Config, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("config: DATABASE_URL は必須です")
	}

	// 接続プールの上限は 1 以上でなければ意味をなさない。
	maxConns, err := int32Env("DB_MAX_CONNS", 20, 1)
	if err != nil {
		return nil, err
	}
	// 下限の 0 は「アイドル接続を事前に張らない」という正当な設定
	// (pgxpool の既定値でもある) なので許可する。
	minConns, err := int32Env("DB_MIN_CONNS", 2, 0)
	if err != nil {
		return nil, err
	}
	if minConns > maxConns {
		return nil, fmt.Errorf("config: DB_MIN_CONNS (%d) が DB_MAX_CONNS (%d) を超えています", minConns, maxConns)
	}

	// 0 は「猶予を設けず即座に終了する」という正当な設定なので許可する。
	shutdownSec, err := intEnv("SHUTDOWN_TIMEOUT_SECONDS", 10, 0)
	if err != nil {
		return nil, err
	}

	debug := strings.EqualFold(os.Getenv("ENV"), "development")

	commentPostMode, err := parseCommentPostMode(os.Getenv("COMMENT_POST_MODE"), debug)
	if err != nil {
		return nil, err
	}

	logFormat, err := parseLogFormat(os.Getenv("LOG_FORMAT"), debug)
	if err != nil {
		return nil, err
	}

	// 既定は 5 秒 (ADR 0006 の「一定間隔 (既定 5 秒)」)。
	// **0 を許さない。** 0 だとスケジューラが既定値 (10 分) へ丸めるので、
	// 「即時に反映されるつもりで 0 にしたら、いちばん遅くなった」が起きる。
	flushSec, err := intEnv("VIEW_COUNT_FLUSH_SECONDS", 5, 1)
	if err != nil {
		return nil, err
	}
	// 既定は 10 分。0 を許すと抑制が無効になるが、それは Phase 4 の
	// 比較で使う設定なので、下限を 0 にして明示的に選べるようにする。
	dedupeSec, err := intEnv("VIEW_COUNT_DEDUPE_SECONDS", 600, 0)
	if err != nil {
		return nil, err
	}

	viewCountMode, err := parseViewCountMode(os.Getenv("VIEW_COUNT_MODE"), debug)
	if err != nil {
		return nil, err
	}

	return &Config{
		Addr:            stringEnv("ADDR", ":8080"),
		DatabaseURL:     dsn,
		MaxConns:        maxConns,
		MinConns:        minConns,
		ShutdownTimeout: time.Duration(shutdownSec) * time.Second,
		AllowedOrigins:  csvEnv("CORS_ALLOWED_ORIGINS", []string{"http://localhost:3000"}),
		Debug:           debug,
		LogFormat:       logFormat,
		CommentPostMode: commentPostMode,
		ViewCount: ViewCountConfig{
			FlushInterval: time.Duration(flushSec) * time.Second,
			DedupeWindow:  time.Duration(dedupeSec) * time.Second,
			Mode:          viewCountMode,
		},
		Auth: AuthConfig{
			GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
			GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			// 既定値を入れない (AuthConfig のコメントを参照)。
			// compose.yaml と apps/go-api/.env.example が明示的に渡す。
			RedirectURL: os.Getenv("AUTH_REDIRECT_URL"),
			FrontendURL: os.Getenv("AUTH_FRONTEND_URL"),
			// シークレットではないが、他の認証設定と同じ経路で渡す。
			// TrimSpace するのは、.env から貼り付けたときの
			// 末尾空白や改行で一致しなくなるのを避けるため。
			// 一致しなければ昇格が起きず、しかも気づきにくい。
			BootstrapAdminGoogleSub: strings.TrimSpace(os.Getenv("BOOTSTRAP_ADMIN_GOOGLE_SUB")),
			// 開発時だけ Secure を外す。未設定の環境は本番扱いで付ける。
			SecureCookie: !debug,
		},
		Storage: StorageConfig{
			Bucket: os.Getenv("S3_BUCKET"),
			// SDK が空リージョンを許さないため、ここだけ既定値を持たせる。
			// **バケットと公開 URL には既定値を入れない** ——
			// AuthConfig の RedirectURL と同じ理由で、入れ忘れに気づけなくなる。
			Region:          stringEnv("S3_REGION", "ap-northeast-1"),
			Endpoint:        os.Getenv("S3_ENDPOINT"),
			UsePathStyle:    boolEnv("S3_USE_PATH_STYLE"),
			AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
			PublicBaseURL:   os.Getenv("S3_PUBLIC_BASE_URL"),
		},
	}, nil
}

// boolEnv は真偽値の環境変数を読み取ります。
//
// **未知の値は false に倒します。** 起動を止めないのは、
// この値が「動くかどうか」ではなく「アクセス形式の選択」であり、
// 誤りは MinIO への接続失敗として即座に現れるためです
// (COMMENT_POST_MODE のように、測定結果を静かに汚す種類の設定とは違います)。
func boolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// csvEnv はカンマ区切りの環境変数を文字列スライスとして読み取ります。
func csvEnv(key string, fallback []string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func stringEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback, minimum int) (int, error) {
	v, err := parseIntEnv(key, int64(fallback), int64(minimum), 64)
	return int(v), err
}

// int32Env は int32 に収まることを保証して読み取ります。
// pgxpool の設定値が int32 なので、ここで範囲を確定させておくと
// 呼び出し側で範囲外を気にする必要がなくなります。
func int32Env(key string, fallback, minimum int32) (int32, error) {
	v, err := parseIntEnv(key, int64(fallback), int64(minimum), 32)
	if err != nil {
		return 0, err
	}
	// bitSize=32 で解析しているので到達しないはずだが、
	// 変換の安全性がコード上で自明になるよう明示的に検査する。
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, fmt.Errorf("config: %s が int32 の範囲を超えています (got %d)", key, v)
	}
	return int32(v), nil
}

// parseIntEnv は環境変数を整数として読み取り、minimum 以上であることを確認します。
// bitSize は strconv.ParseInt に渡す値で、これにより桁あふれを防ぎます。
func parseIntEnv(key string, fallback, minimum int64, bitSize int) (int64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, bitSize)
	if err != nil {
		return 0, fmt.Errorf("config: %s は %d ビット整数である必要があります: %w", key, bitSize, err)
	}
	if v < minimum {
		return 0, fmt.Errorf("config: %s は %d 以上である必要があります (got %d)", key, minimum, v)
	}
	return v, nil
}
