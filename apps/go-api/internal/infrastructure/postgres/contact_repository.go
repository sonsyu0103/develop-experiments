package postgres

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/contact/domain/model"
	"develop-experiments/apps/go-api/internal/contact/domain/repository"
	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
)

// ContactRepository は repository.ContactRepository の PostgreSQL 実装です。
//
// **トランザクションを 1 つも張っていません。** どの操作も 1 文で完結し、
// 束ねる理由がないためです (ReportRepository と同じ形)。
//
// 送信ワーカーの「確保 → 送信 → 反映」だけは複数の文にまたがりますが、
// **1 つのトランザクションにしてはいけません** ——
// SMTP の往復のあいだ行ロックと接続を保持し続けることになります。
// 確保を 1 文で終わらせて即コミットし、送信はその外で行います
// (db/query/contact.sql の ClaimPendingContacts を参照)。
type ContactRepository struct {
	q *sqlcgen.Queries
}

var _ repository.ContactRepository = (*ContactRepository)(nil)

// NewContactRepository は接続プールからリポジトリを生成します。
func NewContactRepository(pool *pgxpool.Pool) *ContactRepository {
	return &ContactRepository{q: sqlcgen.New(pool)}
}

// Save は問い合わせを 1 件保存します。
func (r *ContactRepository) Save(
	ctx context.Context, s model.Submission,
) (*model.Message, error) {
	row, err := r.q.CreateContactMessage(ctx, sqlcgen.CreateContactMessageParams{
		UserID:   s.UserID,
		Name:     s.Name,
		Email:    s.Email,
		Subject:  s.Subject,
		Body:     s.Body,
		ClientIp: toInetPtr(s.ClientIP),
	})
	if err != nil {
		return nil, translateError("ContactRepository.Save", err)
	}

	return &model.Message{
		ID:      row.ID,
		UserID:  s.UserID,
		Name:    s.Name,
		Email:   s.Email,
		Subject: s.Subject,
		Body:    s.Body,
		// 受け付けた直後なので確保は 0 回。
		AttemptCount: 0,
		CreatedAt:    row.CreatedAt,
	}, nil
}

// CountRecent は同じ送信元からの直近 window の件数を返します。
func (r *ContactRepository) CountRecent(
	ctx context.Context, ip netip.Addr, window time.Duration,
) (int64, error) {
	// **無効なアドレスで数えません。** NULL で問い合わせると
	// 等値比較が一致せず、常に 0 件 = 無制限として通ります。
	// 呼び出し側 (Interactor) が手前で弾いていますが、
	// ここでも閉じておきます —— 素通しは失敗として見えないためです。
	if !ip.IsValid() {
		return 0, nil
	}

	n, err := r.q.CountRecentContacts(ctx, sqlcgen.CountRecentContactsParams{
		ClientIp: toInetPtr(ip),
		Window:   toInterval(window),
	})
	if err != nil {
		return 0, translateError("ContactRepository.CountRecent", err)
	}
	return n, nil
}

// ClaimPending は送信する行を確保します。
func (r *ContactRepository) ClaimPending(
	ctx context.Context, lease time.Duration, batchSize int32,
) ([]model.Message, error) {
	rows, err := r.q.ClaimPendingContacts(ctx, sqlcgen.ClaimPendingContactsParams{
		Lease:     toInterval(lease),
		BatchSize: clampMaxRows(batchSize),
	})
	if err != nil {
		return nil, translateError("ContactRepository.ClaimPending", err)
	}

	messages := make([]model.Message, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, model.Message{
			ID:           row.ID,
			UserID:       row.UserID,
			Name:         row.Name,
			Email:        row.Email,
			Subject:      row.Subject,
			Body:         row.Body,
			AttemptCount: row.AttemptCount,
			CreatedAt:    row.CreatedAt,
			ReplyTo:      verifiedEmailOf(ctx, row.ID, row.VerifiedEmail),
		})
	}
	return messages, nil
}

// verifiedEmailOf は users.email を自動返信の宛先に変換します。
//
// **このシステムで VerifiedEmail が生まれる唯一の場所です**
// (ADR 0008 決定 2)。所有権を保証しているのは users.email 自体で、
// 根拠は 2 つあります (ADR 0005 決定 3)。
//
//	IdP が email_verified = true として渡した値しか受け入れていない
//	ログインのたびに UpsertUser が書き直している (古いまま残らない)
//
// **利用者がフォームに入力した contact_messages.email はここへ来ません。**
// 引数は LEFT JOIN した users 側の列だけです。
//
// **形式が不正なら nil を返して、自動返信を送らないだけにします。**
// users.email に CHECK 制約は無く、IdP が返した文字列がそのまま入ります。
// ここでエラーを返すと、**行 1 つのせいで確保ごと止まり、運営への通知が
// 丸ごと遅れます** —— 控えが 1 通届かないほうが軽い。
func verifiedEmailOf(ctx context.Context, contactID int64, raw *string) *model.VerifiedEmail {
	// NULL = 匿名の問い合わせ、または退会済みの利用者。
	if raw == nil {
		return nil
	}

	v, err := model.NewVerifiedEmail(*raw)
	if err != nil {
		// **アドレスもエラー文も載せません** (ADR 0010 の 4-5)。
		// エラー文には不正だった値そのものが含まれ、それは個人データです。
		// 突き合わせに要るのは contact_id だけになります。
		slog.WarnContext(ctx, "contact_verified_email_invalid",
			slog.Int64("contact_id", contactID))
		return nil
	}
	return &v
}

// MarkSent は送信できた行を確定します。
//
// **0 行でもエラーにしません。** リース切れで 2 つのワーカーが同じ行を
// 送った場合、後から来たほうが 0 行になります —— 送信自体は済んでおり、
// 呼び出し側にできることもありません (at-least-once として引き受けた形)。
// 記録だけ残します。
func (r *ContactRepository) MarkSent(ctx context.Context, id int64) error {
	return r.applyResult(ctx, "ContactRepository.MarkSent", id, func() (int64, error) {
		return r.q.MarkContactSent(ctx, id)
	})
}

// Reschedule は失敗した行を次の試行へ回します。
func (r *ContactRepository) Reschedule(
	ctx context.Context, id int64, backoff time.Duration, lastErr string,
) error {
	return r.applyResult(ctx, "ContactRepository.Reschedule", id, func() (int64, error) {
		return r.q.RescheduleContact(ctx, sqlcgen.RescheduleContactParams{
			ID:        id,
			Backoff:   toInterval(backoff),
			LastError: nullableText(lastErr),
		})
	})
}

// Fail は試行回数の上限を超えた行を打ち切ります。
func (r *ContactRepository) Fail(ctx context.Context, id int64, lastErr string) error {
	return r.applyResult(ctx, "ContactRepository.Fail", id, func() (int64, error) {
		return r.q.FailContact(ctx, sqlcgen.FailContactParams{
			ID:        id,
			LastError: nullableText(lastErr),
		})
	})
}

// applyResult は送信結果の反映 3 種に共通する扱いです。
//
// **0 行を「無い」(404) に翻訳しません。** これらの UPDATE は
// `status = 'pending'` を条件に含めており、0 行は
// 「他のワーカーが先に確定した」を意味します。呼び出し側は定期処理なので、
// 404 を受け取っても何もできません —— 記録に留めます。
func (r *ContactRepository) applyResult(
	ctx context.Context, op string, id int64, exec func() (int64, error),
) error {
	n, err := exec()
	if err != nil {
		return translateError(op, err)
	}
	if n == 0 {
		logContactRaced(ctx, op, id)
	}
	return nil
}

// logContactRaced は「先に確定されていた」ことを記録します。
//
// **INFO です。** 起きても正しい状態 (誰かが確定した) であり、
// 人が対応するものではありません (ADR 0010 の 4-3)。
// ただし頻発するならリースが短すぎる合図になるので、数えられるようにします。
func logContactRaced(ctx context.Context, op string, id int64) {
	slog.InfoContext(ctx, "contact_result_raced",
		slog.String("op", op),
		slog.Int64("contact_id", id),
	)
}

// OldestPending は最も古い未送信の受付時刻を返します。
func (r *ContactRepository) OldestPending(ctx context.Context) (time.Time, bool, error) {
	oldest, err := r.q.OldestPendingContact(ctx)
	if err != nil {
		// **0 行は正常です。** 未送信が無いということなので、
		// translateError に通して 404 にしてはいけません。
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, translateError("ContactRepository.OldestPending", err)
	}
	return oldest, true, nil
}

// ScrubClientIPs は保持期間を過ぎた行から送信元を消します。
func (r *ContactRepository) ScrubClientIPs(
	ctx context.Context, retention time.Duration, maxRows int32,
) (int64, error) {
	n, err := r.q.ScrubContactClientIPs(ctx, sqlcgen.ScrubContactClientIPsParams{
		Retention: toInterval(retention),
		MaxRows:   clampMaxRows(maxRows),
	})
	if err != nil {
		return 0, translateError("ContactRepository.ScrubClientIPs", err)
	}
	return n, nil
}

// toInetPtr は netip.Addr を inet 列のパラメータへ変換します。
//
// **ゼロ値は NULL にします。** netip.Addr のゼロ値をそのまま渡すと
// pgx が不正なアドレスとして扱い、保存が失敗します。
// 「送信元が分からない問い合わせ」は正常に存在するので、
// 落とさず NULL として保存します。
func toInetPtr(ip netip.Addr) *netip.Addr {
	if !ip.IsValid() {
		return nil
	}
	// **Unmap しています。** IPv4 射影された IPv6 (::ffff:192.0.2.1) と
	// 素の IPv4 (192.0.2.1) は netip では別の値ですが、PostgreSQL の inet に
	// 入れると別の表現のまま保存され、**同じ送信元が別人として数えられます。**
	// レート制限はずれると意味を失うので、入口で 1 つに寄せます。
	unmapped := ip.Unmap()
	return &unmapped
}

// nullableText は空文字を NULL にします。
//
// last_error は「直近の失敗理由」で、空文字は理由が無いことと同じです。
// 空文字のまま入れると、SQL 側で `last_error IS NOT NULL` が
// 「失敗した行」を拾えなくなります。
func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
