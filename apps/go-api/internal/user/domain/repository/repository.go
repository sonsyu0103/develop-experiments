// Package repository は利用者とセッションの永続化に対するインターフェースを定義します。
//
// インターフェースをドメイン側に置き、実装を infrastructure 側に置くことで、
// ユースケース層が PostgreSQL にも sqlc にも依存しなくなります (依存性逆転)。
package repository

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/user/domain/model"
)

// ErrWithdrawn は退会済みの利用者に一致したことを表します。
//
// apperr.ErrNotFound と分けているのは、HTTP 層での扱いが違うためです。
// ErrNotFound をそのまま返すと、退会済みの再ログインが 404 になり、
// 「そんなアカウントは無い」と「そのアカウントは閉じられている」を
// クライアントが区別できません。
//
// 再ログインを拒否するか復帰させるかは未決です
// (docs/adr/0005-authentication.md)。どちらに倒す場合でも、
// 呼び出し側が分岐できる形にしておく必要があります。
var ErrWithdrawn = errors.New("退会済みの利用者です")

// UserRepository は利用者の読み書きを担います。
type UserRepository interface {
	// Upsert はログイン時に呼び、google_sub で照合して無ければ作成します。
	//
	// 退会済みの利用者に一致した場合は ErrWithdrawn を返します。
	// 「復活させる」か「拒否する」かが未決のため、黙って復活させない側に
	// 倒してあります (db/query/users.sql の UpsertUser を参照)。
	Upsert(ctx context.Context, user *model.User) (*model.User, error)

	// FindByID は内部 ID で取得します。退会済みは見つかりません。
	FindByID(ctx context.Context, id int64) (*model.User, error)

	// FindByPublicID は公開用の識別子で取得します。
	// API から来る識別子は常にこちらです。
	FindByPublicID(ctx context.Context, publicID uuid.UUID) (*model.User, error)

	// PromoteToAdmin は google_sub で指定した利用者を admin にします。
	//
	// **最初の管理者を作る唯一の経路**です (ADR 0011 決定 1)。
	// UI から作れる形にすると、それがそのまま
	// 「誰でも管理者になれる」機能になりえます。
	//
	// 既に admin の場合は何もせず、promoted が false になります。
	// 該当する利用者が居ない場合も false です (エラーにしません ——
	// 環境変数に未ログインの sub を書いておく運用が成り立つため)。
	PromoteToAdmin(ctx context.Context, googleSub string) (promoted bool, err error)

	// ListAuthorsByIDs は投稿者の表示に必要な情報だけをまとめて取得します。
	//
	// 【現時点で本番経路からは呼ばれていません】
	// 投稿者の解決は LEFT JOIN で行っています
	// (docs/adr/0014-author-resolution.md の選択肢 A)。
	// これは選択肢 B (一括取得) の部品で、
	// 「コメント一覧では B が勝つ可能性がある」を Phase 4 で測るために残しています。
	// 測って A のままなら、そのときに消します。
	//
	// 投稿一覧の投稿者表示で使います。1 件ずつ引くと N+1 になるため、
	// 呼び出し側は author_id を集めてから 1 回で引いてください。
	//
	// model.User ではなく model.Author を返すのは、この値の行き先が
	// 他人にも見える一覧だからです。email や google_sub を運ばない形にしてあります。
	//
	// 退会済みの利用者も含めて返します —— 投稿は匿名化されるまで残るため、
	// 「退会したので表示を変える」を呼び出し側が判断できる必要があります。
	ListAuthorsByIDs(ctx context.Context, ids []int64) ([]model.Author, error)
}

// SessionRepository はセッションの読み書きを担います。
type SessionRepository interface {
	// Create はセッションを保存します。
	Create(ctx context.Context, session *model.Session) (*model.Session, error)

	// FindLive は有効なセッションを、持ち主の情報つきで取得します。
	//
	// 受け取るのは Cookie から来た**生のトークン**です。
	// DB に保存されているのはそのハッシュなので、変換は実装側で行います
	// —— 呼び出し側がハッシュ化を忘れる余地を残さないためです。
	//
	// 期限切れ・退会済みの場合は apperr.ErrNotFound を返します。
	// この判定は SQL 側に置いてあり、アプリ側では行いません。
	FindLive(ctx context.Context, token model.SessionToken) (*model.AuthenticatedSession, error)

	// SetAvatarImage はプロフィール画像を設定します。
	//
	// imageID が nil なら解除します (Google のプロフィール画像に戻ります)。
	// **所有者の確認は行いません** —— 他人の画像を 404 として扱う必要があり、
	// ここで外部キーに弾かせると 400 になってしまうためです。
	// 確認はユースケース層が先に行います。
	SetAvatarImage(ctx context.Context, userID int64, imageID *uuid.UUID) (*model.SessionOwner, error)

	// Delete は 1 件のセッションを削除します (ログアウト)。
	// FindLive と同じく生のトークンを受け取ります。
	//
	// 対象が無い場合も成功として扱います —— 目的は「もう使えないこと」であり、
	// 既に無いならその目的は達成されているためです。
	Delete(ctx context.Context, token model.SessionToken) error

	// DeleteByUserID は利用者のセッションをすべて削除します
	// (全端末からのログアウト、権限剥奪の直後)。削除件数を返します。
	DeleteByUserID(ctx context.Context, userID int64) (int64, error)

	// DeleteExpired は期限切れのセッションを最大 maxRows 件削除し、
	// 削除件数を返します。定期処理から「0 件になるまで」呼びます。
	//
	// 一度に消す件数を区切るのは、放置後に大量にたまっていた場合でも
	// 1 トランザクションを短く保つためです。
	//
	// maxRows が 0 以下の場合は既定値に丸めます。
	// LIMIT 0 をそのまま流すと 1 件も消えず、しかも戻り値が 0 になるため、
	// **「0 件になるまで繰り返す」という呼び出し規約の終了条件と区別がつきません。**
	// 掃除が動いていないことに誰も気づけない形になります。
	DeleteExpired(ctx context.Context, maxRows int32) (int64, error)
}
