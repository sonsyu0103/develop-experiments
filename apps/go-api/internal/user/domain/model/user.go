// Package model は利用者と認証セッションのドメインモデルを定義します。
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
)

// DisplayNameMaxLength は表示名の最大文字数です。
// DB 側の CHECK 制約 (users_display_name_length) と必ず同じ値にしてください。
//
// 匿名投稿の author_name (50 文字) より緩いのは、この値が Google から来る
// 外部入力であるためです。揃えると、長い表示名の利用者がログインできなくなります。
// 切り詰めはアプリ側の責任で、DB は暴走を止める上限として持ちます
// (docs/adr/0016-schema-and-indexes.md)。
const DisplayNameMaxLength = 100

// normalizeDisplayName は Google の表示名を、保存できる形に整えます。
//
// **弾かずに直します。** 外部から来る値なので、利用者には直しようがありません。
//
//	空          -> google_sub から導いた代替名を使う
//	             (profile スコープが無いと name は空になりえます)
//	長すぎる    -> DisplayNameMaxLength 文字で切る
//	             (DB の CHECK は「暴走を止める上限」で、切るのはこちらの責任)
//
// **メールアドレスは使いません。** DisplayName は Author に載って
// 未ログインの一覧にも出ます。仕様書は Me.email について
// 「本人にだけ返します。投稿一覧には含まれません」と約束しており、
// ローカル部だけでも個人を指す文字列がそこへ混ざります
// (初版はローカル部で代用していました。レビュー指摘)。
func normalizeDisplayName(displayName, googleSub string) string {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = fallbackDisplayName(googleSub)
	}

	if utf8.RuneCountInString(displayName) > DisplayNameMaxLength {
		displayName = trimDanglingSequence([]rune(displayName)[:DisplayNameMaxLength])
	}
	return displayName
}

// fallbackDisplayName は表示名が取れないときの代替です。
//
// **google_sub のハッシュから作ります。** 満たしたい性質が 3 つあります。
//
//	一意である      同じ名前が並ばない
//	毎回同じである  Upsert が display_name を上書きするので、ログインの
//	                たびに変わると表示名が揺れます。**public_id は使えません**
//	                —— NewUser は毎回新しい値を採番するため
//	復元できない    google_sub をそのまま出すと外部の識別子が公開されます
func fallbackDisplayName(googleSub string) string {
	sum := sha256.Sum256([]byte(googleSub))
	return "利用者-" + hex.EncodeToString(sum[:4])
}

// trimDanglingSequence は、切った末尾に残った「途中」の記号を落とします。
//
// **文字 (rune) 単位で切っても、絵文字は割れます。** 家族の絵文字は
// ZWJ (U+200D) で複数の絵文字をつないだ列で、旗は地域指示符号の 2 個組です。
// 境界で切ると、末尾に**行き場のない ZWJ** や地域指示符号が 1 個だけ残り、
// 豆腐 (□) として表示されます。
//
// 書記素クラスタまで正確に扱うには外部ライブラリが要るので、
// **末尾の破片を落とすところまで**にしています。
func trimDanglingSequence(rs []rune) string {
	for len(rs) > 0 {
		last := rs[len(rs)-1]
		switch {
		case last == '\u200d': // ZWJ。次の絵文字が来るはずだった
		case last == '\ufe0f' || last == '\ufe0e': // 異体字セレクタ
		case isRegionalIndicator(last) && countTrailingRegionalIndicators(rs)%2 == 1:
		default:
			return string(rs)
		}
		rs = rs[:len(rs)-1]
	}
	return ""
}

// isRegionalIndicator は旗を作る地域指示符号かを返します (2 個で 1 つの旗)。
func isRegionalIndicator(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

func countTrailingRegionalIndicators(rs []rune) int {
	n := 0
	for i := len(rs) - 1; i >= 0 && isRegionalIndicator(rs[i]); i-- {
		n++
	}
	return n
}

// User は Google アカウントに紐づく利用者を表すエンティティです。
//
// ID は内部 ID で、API には出しません。外部に見せるのは PublicID だけです
// (docs/adr/0003-open-questions.md 未決 #11 の決定)。
type User struct {
	ID          int64
	PublicID    uuid.UUID
	GoogleSub   string
	Email       string
	DisplayName string
	AvatarURL   *string
	// Role は権限です。**新規登録では常に RoleUser** になります
	// (DB 側の DEFAULT)。クライアントから来た値をここに入れる経路は
	// 作りません (docs/adr/0011-moderation.md 決定 1)。
	Role      Role
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// Author は投稿一覧で投稿者を表示するための、最小限の情報です。
//
// GoogleSub も Email も持ちません。この値の行き先は
// **他人にも見える投稿一覧**であり、余分な列を運ぶと、
// 詰め替えを 1 つ間違えただけで読み手に渡ります
// (docs/adr/0014-author-resolution.md)。
type Author struct {
	ID          int64
	PublicID    uuid.UUID
	DisplayName string
	AvatarURL   *string
	// DeletedAt は退会済みかの判定に使います。
	// 投稿は匿名化されるまで残るため、表示側で扱いを変える必要があります。
	DeletedAt *time.Time
}

// IsDeleted は退会済みかを返します。
func (a *Author) IsDeleted() bool {
	return a.DeletedAt != nil
}

// SessionOwner はセッション検証で必要になる範囲の利用者情報です。
//
// GoogleSub を含めていません。あれは IdP との照合にだけ使う値であり、
// リクエストのたびに持ち回ると、ログや文脈に載る機会が無駄に増えます。
type SessionOwner struct {
	ID          int64
	PublicID    uuid.UUID
	Email       string
	DisplayName string
	AvatarURL   *string
	// Role は権限判定に使います。毎リクエストの経路で必要になるため、
	// セッションの検証と同じクエリで引いています (db/query/sessions.sql)。
	Role Role
	// AvatarObjectKey はアップロードしたプロフィール画像のキーです。
	// 設定していなければ nil で、そのとき AvatarURL (Google のもの) を使います
	// (docs/adr/0007-image-storage.md のスキーマ)。
	//
	// **URL ではなくキーを持ちます。** 組み立ては環境ごとの設定を要するため、
	// ドメインの外 (ユースケース層) で行います。
	AvatarObjectKey *string
}

// NewUser は永続化前の新しい利用者を組み立てます。
// ID と各種時刻は DB が採番するため、ここでは設定しません。
//
// PublicID はここで採番します。UUID v7 を使うのは、先頭 48 ビットが
// タイムスタンプなので B-tree の挿入位置が末尾に寄り、
// v4 のようなページ分割が起きにくいためです。
// PostgreSQL 17 には uuidv7() が無いため生成は Go 側で行います。
func NewUser(googleSub, email, displayName string, avatarURL *string) (*User, error) {
	googleSub = strings.TrimSpace(googleSub)
	if googleSub == "" {
		return nil, fmt.Errorf("google_sub が空です: %w", apperr.ErrInvalidArgument)
	}

	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("メールアドレスが空です: %w", apperr.ErrInvalidArgument)
	}

	// **表示名で login を落とさない。** ここに来る値は Google が返す claims で、
	// 利用者がこのアプリから直せるものではありません。長い・空を弾くと
	// **その人は永久にログインできず、しかも直す手段がありません**
	// (呼び出し側は ErrUnauthenticated ではないので login_failed になる)。
	//
	// この型のコメントも「切り詰めはアプリ側の責任」と書いていたのに、
	// **実装は弾く側になっていました** (レビュー指摘)。
	displayName = normalizeDisplayName(displayName, googleSub)
	if displayName == "" {
		return nil, fmt.Errorf("表示名が空です: %w", apperr.ErrInvalidArgument)
	}

	publicID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("public_id の生成に失敗しました: %w", err)
	}

	return &User{
		PublicID:    publicID,
		GoogleSub:   googleSub,
		Email:       email,
		DisplayName: displayName,
		AvatarURL:   avatarURL,
	}, nil
}

// Reconstruct は永続化層から読み出した値で利用者を復元します。
// 保存済みのデータが対象なので、検証は行いません。
func Reconstruct(
	id int64, publicID uuid.UUID, googleSub, email, displayName string,
	avatarURL *string, role Role, createdAt, updatedAt time.Time, deletedAt *time.Time,
) *User {
	return &User{
		ID:          id,
		PublicID:    publicID,
		GoogleSub:   googleSub,
		Email:       email,
		DisplayName: displayName,
		AvatarURL:   avatarURL,
		Role:        role,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		DeletedAt:   deletedAt,
	}
}

// IsDeleted は退会済みかを返します。
//
// 退会しても行は消しません。投稿の author_id から参照されており、
// 削除は匿名化 (author_id を NULL にする) で行うためです
// (docs/adr/0005-authentication.md)。
func (u *User) IsDeleted() bool {
	return u.DeletedAt != nil
}
