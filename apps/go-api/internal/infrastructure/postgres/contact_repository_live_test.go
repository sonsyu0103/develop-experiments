package postgres

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/contact/domain/model"
)

// ---------------------------------------------------------------------------
// 実 DB に対する検証: 問い合わせ (docs/adr/0008-contact-and-mail.md)
// ---------------------------------------------------------------------------
//
// **フェイクでは測れないものが 4 つあります。**
//
//   - `FOR UPDATE SKIP LOCKED` が、同時に走った 2 つの確保に
//     **別々の行を配ること。** 二重送信を防いでいる仕組みそのもので、
//     これはロックの挙動なのでフェイクでは再現できません
//   - 確保が **next_attempt_at を先送りする**こと (リース)。
//     先送りしないと、送信中の行を次の周回が拾い直します
//   - 送信結果の反映が `status = 'pending'` を条件にしていること
//   - `client_ip` が INET として正規化されること
//     (レート制限は表現がずれると意味を失います)

// cleanupContacts は検証で積んだ問い合わせを消します。
func cleanupContacts(t *testing.T, pool *pgxpool.Pool, subject string) {
	t.Helper()

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM contact_messages WHERE subject = $1`, subject)
	})
}

// insertContact は未送信の問い合わせを 1 件作ります。
func insertContact(t *testing.T, pool *pgxpool.Pool, subject string) int64 {
	t.Helper()

	var id int64
	err := pool.QueryRow(t.Context(), `
		INSERT INTO contact_messages (name, email, subject, body)
		VALUES ('live テスト', 'live@example.com', $1, '本文')
		RETURNING id`, subject).Scan(&id)
	if err != nil {
		t.Fatalf("問い合わせを作れませんでした: %v", err)
	}
	return id
}

// **ロック中の行を、待たずに飛ばすこと。**
//
// ADR 0008 決定 1 の「複数インスタンスでの二重送信を防ぐ」の本体です。
//
// 【初版はこれを検査できていませんでした】(レビュー指摘)
// 確保を 2 回**続けて**呼んでいたので、2 回目が走る時点で 1 回目は
// 既にコミット済みでした。**ロックされた行が 1 つも無い状態**なので
// SKIP LOCKED に到達せず、実際に見ていたのはリースの効果だけです ——
// クエリから SKIP LOCKED を消しても緑のままでした。
//
// そこで**別のトランザクションで明示的に行ロックを取ってから**確保します。
// SKIP LOCKED が無ければ、この確保はロックの解放まで**待ち**、
// 締め切り (下の 5 秒) に当たって失敗します。
func TestContactRepository_ClaimSkipsLocked_Live(t *testing.T) {
	pool := liveDB(t)
	const subject = "live-contact-skip-locked"
	cleanupContacts(t, pool, subject)

	locked := insertContact(t, pool, subject)
	others := map[int64]bool{
		insertContact(t, pool, subject): true,
		insertContact(t, pool, subject): true,
	}

	// **別の接続で行ロックを取り、保持したままにします。**
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("トランザクションを開始できない: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, lockErr := tx.Exec(t.Context(),
		`SELECT id FROM contact_messages WHERE id = $1 FOR UPDATE`, locked); lockErr != nil {
		t.Fatalf("行ロックを取れない: %v", lockErr)
	}

	// **締め切りを付けるのが要点。** SKIP LOCKED が無ければここで待たされ、
	// 5 秒後に context deadline exceeded になります。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	repo := NewContactRepository(pool)
	claimed, err := repo.ClaimPending(ctx, 5*time.Minute, 10)
	if err != nil {
		t.Fatalf("ロック中の行を待ってしまった (SKIP LOCKED が効いていない): %v", err)
	}

	if len(claimed) != len(others) {
		t.Fatalf("確保できた件数 = %d, want %d", len(claimed), len(others))
	}
	for _, m := range claimed {
		if m.ID == locked {
			t.Error("ロック中の行が配られた")
		}
		if !others[m.ID] {
			t.Errorf("知らない行が返った: id = %d", m.ID)
		}
		// **確保の時点で attempt_count が増えていること。**
		// 増やさないと、クラッシュを繰り返す行が上限に達せず居座ります。
		if m.AttemptCount != 1 {
			t.Errorf("attempt_count = %d, want 1", m.AttemptCount)
		}
	}

	// **飛ばされただけで、消えてはいないこと。**
	// ロックを解けば、次の周回で拾われます。
	if rollbackErr := tx.Rollback(t.Context()); rollbackErr != nil {
		t.Fatalf("ロックを解けない: %v", rollbackErr)
	}
	after, err := repo.ClaimPending(t.Context(), 5*time.Minute, 10)
	if err != nil {
		t.Fatalf("確保に失敗: %v", err)
	}
	if len(after) != 1 || after[0].ID != locked {
		t.Errorf("飛ばした行が拾い直されていない: %+v", after)
	}
}

// **確保した行はリースのあいだ他から見えないこと。**
//
// SKIP LOCKED とは別の仕組みです。確保は 1 文で終わってコミットするので、
// **送信中の行を隠しているのはロックではなく next_attempt_at の先送り**に
// なります。これが効いていないと、送信中の行を次の周回が拾い直します。
func TestContactRepository_ClaimLeases_Live(t *testing.T) {
	pool := liveDB(t)
	const subject = "live-contact-lease"
	cleanupContacts(t, pool, subject)

	want := map[int64]bool{}
	for range 4 {
		want[insertContact(t, pool, subject)] = true
	}

	repo := NewContactRepository(pool)

	first, err := repo.ClaimPending(t.Context(), 5*time.Minute, 2)
	if err != nil {
		t.Fatalf("1 回目の確保に失敗: %v", err)
	}
	second, err := repo.ClaimPending(t.Context(), 5*time.Minute, 2)
	if err != nil {
		t.Fatalf("2 回目の確保に失敗: %v", err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("確保できた件数 = %d, %d (want 2, 2)", len(first), len(second))
	}

	seen := map[int64]bool{}
	for _, m := range append(append([]model.Message{}, first...), second...) {
		if seen[m.ID] {
			t.Errorf("同じ行が 2 回配られた: id = %d", m.ID)
		}
		seen[m.ID] = true
		if !want[m.ID] {
			t.Errorf("知らない行が返った: id = %d", m.ID)
		}
	}

	// **3 回目は空。** 4 件すべてがリース中になっている。
	third, err := repo.ClaimPending(t.Context(), 5*time.Minute, 10)
	if err != nil {
		t.Fatalf("3 回目の確保に失敗: %v", err)
	}
	if len(third) != 0 {
		t.Errorf("リース中の行が再び配られた: %d 件", len(third))
	}
}

// **送信結果の反映が pending の行にだけ効くこと。**
//
// 条件に含めないと、リース切れで 2 つのワーカーが同じ行を送ったとき、
// 後から来たほうが送信時刻を上書きします。
func TestContactRepository_MarkSentIsIdempotent_Live(t *testing.T) {
	pool := liveDB(t)
	const subject = "live-contact-mark-sent"
	cleanupContacts(t, pool, subject)

	id := insertContact(t, pool, subject)
	repo := NewContactRepository(pool)

	if err := repo.MarkSent(t.Context(), id); err != nil {
		t.Fatalf("確定に失敗: %v", err)
	}

	var status string
	var sentAt *time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT status, sent_at FROM contact_messages WHERE id = $1`, id,
	).Scan(&status, &sentAt); err != nil {
		t.Fatalf("読み直せない: %v", err)
	}
	if status != string(model.StatusSent) || sentAt == nil {
		t.Fatalf("status = %q, sent_at = %v", status, sentAt)
	}
	firstSentAt := *sentAt

	// **2 回目は 0 行。** エラーにはならず、値も動かない。
	if err := repo.MarkSent(t.Context(), id); err != nil {
		t.Fatalf("2 回目の確定がエラーになった: %v", err)
	}
	if err := pool.QueryRow(t.Context(),
		`SELECT sent_at FROM contact_messages WHERE id = $1`, id).Scan(&sentAt); err != nil {
		t.Fatalf("読み直せない: %v", err)
	}
	if !sentAt.Equal(firstSentAt) {
		t.Errorf("送信時刻が上書きされた: %v -> %v", firstSentAt, *sentAt)
	}

	// 送信済みの行は確保されない (部分索引の述語どおり)。
	claimed, err := repo.ClaimPending(t.Context(), time.Minute, 10)
	if err != nil {
		t.Fatalf("確保に失敗: %v", err)
	}
	for _, m := range claimed {
		if m.ID == id {
			t.Error("送信済みの行が確保された")
		}
	}
}

// **打ち切った行が再送されないこと** (ADR 0008 決定 1)。
func TestContactRepository_Fail_Live(t *testing.T) {
	pool := liveDB(t)
	const subject = "live-contact-fail"
	cleanupContacts(t, pool, subject)

	id := insertContact(t, pool, subject)
	repo := NewContactRepository(pool)

	if err := repo.Fail(t.Context(), id, "送れませんでした"); err != nil {
		t.Fatalf("打ち切りに失敗: %v", err)
	}

	var status string
	var lastErr *string
	if err := pool.QueryRow(t.Context(),
		`SELECT status, last_error FROM contact_messages WHERE id = $1`, id,
	).Scan(&status, &lastErr); err != nil {
		t.Fatalf("読み直せない: %v", err)
	}
	if status != string(model.StatusFailed) {
		t.Errorf("status = %q, want failed", status)
	}
	// **行は消えていないこと。** 消すと運用が手で拾い直せなくなります。
	if lastErr == nil || *lastErr != "送れませんでした" {
		t.Errorf("失敗理由が残っていない: %v", lastErr)
	}
}

// **レート制限が INET の表現差に影響されないこと。**
//
// TEXT で持つと "192.168.0.1" と "192.168.000.001" が別の値になり、
// 同じ送信元が別人として数えられます。
// IPv4 射影された IPv6 (::ffff:…) も同じ問題を起こします。
func TestContactRepository_CountRecent_Live(t *testing.T) {
	pool := liveDB(t)
	const subject = "live-contact-rate"
	cleanupContacts(t, pool, subject)

	repo := NewContactRepository(pool)
	ip := netip.MustParseAddr("198.51.100.7")

	for range 3 {
		if _, err := repo.Save(t.Context(), model.Submission{
			Name: "live テスト", Email: "live@example.com",
			Subject: subject, Body: "本文", ClientIP: ip,
		}); err != nil {
			t.Fatalf("保存に失敗: %v", err)
		}
	}

	n, err := repo.CountRecent(t.Context(), ip, time.Hour)
	if err != nil {
		t.Fatalf("数えられなかった: %v", err)
	}
	if n != 3 {
		t.Errorf("件数 = %d, want 3", n)
	}

	// **IPv4 射影された IPv6 でも同じ行が数えられること。**
	// 別々に数えられると、v6 で送るだけでレート制限を回避できます。
	mapped := netip.MustParseAddr("::ffff:198.51.100.7")
	n, err = repo.CountRecent(t.Context(), mapped, time.Hour)
	if err != nil {
		t.Fatalf("数えられなかった: %v", err)
	}
	if n != 3 {
		t.Errorf("射影された表現での件数 = %d, want 3", n)
	}

	// 別の送信元は数に入らない。
	other, err := repo.CountRecent(t.Context(), netip.MustParseAddr("198.51.100.8"), time.Hour)
	if err != nil {
		t.Fatalf("数えられなかった: %v", err)
	}
	if other != 0 {
		t.Errorf("別の送信元の件数 = %d, want 0", other)
	}
}

// **消し込みが古い行の送信元だけを消すこと。**
//
// 行そのものは残します —— 消すと問い合わせの記録が消えます。
func TestContactRepository_ScrubClientIPs_Live(t *testing.T) {
	pool := liveDB(t)
	const subject = "live-contact-scrub"
	cleanupContacts(t, pool, subject)

	repo := NewContactRepository(pool)
	ip := netip.MustParseAddr("203.0.113.9")
	saved, err := repo.Save(t.Context(), model.Submission{
		Name: "live テスト", Email: "live@example.com",
		Subject: subject, Body: "本文", ClientIP: ip,
	})
	if err != nil {
		t.Fatalf("保存に失敗: %v", err)
	}

	// **保持期間内は消えないこと。**
	n, err := repo.ScrubClientIPs(t.Context(), 400*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("消し込みに失敗: %v", err)
	}
	if n != 0 {
		t.Errorf("保持期間内の行が消された: %d 件", n)
	}

	// 保持期間 0 なら対象になる。
	n, err = repo.ScrubClientIPs(t.Context(), 0, 100)
	if err != nil {
		t.Fatalf("消し込みに失敗: %v", err)
	}
	if n == 0 {
		t.Fatal("消し込まれなかった")
	}

	var clientIP *netip.Addr
	var body string
	if err := pool.QueryRow(t.Context(),
		`SELECT client_ip, body FROM contact_messages WHERE id = $1`, saved.ID,
	).Scan(&clientIP, &body); err != nil {
		t.Fatalf("読み直せない: %v", err)
	}
	if clientIP != nil {
		t.Errorf("送信元が残っている: %v", *clientIP)
	}
	// **行は残っていること。**
	if body != "本文" {
		t.Errorf("行ごと消えた (body = %q)", body)
	}
}

// **最も古い未送信が読めること** (ADR 0008 の「引き受けるコスト」)。
//
// これが無いと、送信が止まっても誰も気づきません。
func TestContactRepository_OldestPending_Live(t *testing.T) {
	pool := liveDB(t)
	const subject = "live-contact-oldest"
	cleanupContacts(t, pool, subject)

	repo := NewContactRepository(pool)
	id := insertContact(t, pool, subject)

	oldest, ok, err := repo.OldestPending(t.Context())
	if err != nil {
		t.Fatalf("読めなかった: %v", err)
	}
	if !ok {
		t.Fatal("未送信があるのに ok = false")
	}
	if oldest.IsZero() {
		t.Error("時刻が空")
	}

	// **0 行は 404 ではないこと。** translateError に通すと
	// 「未送信が無い」が「見つからない」というエラーになります。
	if _, err := pool.Exec(t.Context(),
		`UPDATE contact_messages SET status = 'failed' WHERE id = $1`, id); err != nil {
		t.Fatalf("更新に失敗: %v", err)
	}
	// 他のテストが積んだ pending が残っていることがあるので、
	// ここでは「エラーにならないこと」だけを見ます。
	if _, _, err := repo.OldestPending(t.Context()); err != nil {
		t.Errorf("未送信が無い状態でエラーになった: %v", err)
	}
}
