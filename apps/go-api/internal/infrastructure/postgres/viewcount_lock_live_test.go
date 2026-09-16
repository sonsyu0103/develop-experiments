package postgres

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 実 DB に対する検証: 閲覧数の反映とコメント投稿のロック (DB レビュー)
// ---------------------------------------------------------------------------
//
// **ロックの強さはフェイクでは測れません。** 衝突するかどうかは
// PostgreSQL の行ロックの衝突表で決まり、Go 側には対応するコードがありません。

// **投稿トランザクションが開いていても、閲覧数の反映が待たされないこと。**
//
// コメントの INSERT は外部キーの確認で親スレッドの行に FOR KEY SHARE を取ります。
// IncrementThreadViewCounts の CTE が FOR UPDATE だとこれと衝突し、
// 反映が投稿の終了を待ちます。id 昇順にロックを取っていく途中で待つので、
// **それまでにロックした無関係なスレッドへの投稿まで玉突きで待たされます**
// (実測: 4 秒かかる投稿 1 本で、別スレッドへの投稿が 2,947 ms)。
//
// FOR NO KEY UPDATE なら衝突しません。ここでは反映に 2 秒の締め切りを付け、
// 開いたままの投稿トランザクションを越えて終わることを確かめます。
func TestIncrementViewCounts_DoesNotWaitForOpenCommentInsert_Live(t *testing.T) {
	pool := liveDB(t)
	ctx := t.Context()

	threadID := seedThread(t, pool, nil)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("トランザクションを開始できませんでした: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, execErr := tx.Exec(ctx, `
		INSERT INTO comments (thread_id, seq, author_name, body)
		VALUES ($1, 1, '名無しさん', 'live: 閲覧数の反映と同時に開いている投稿')`,
		threadID); execErr != nil {
		t.Fatalf("投稿を挿入できませんでした: %v", execErr)
	}

	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	affected, err := NewThreadRepository(pool).IncrementViewCounts(
		callCtx, []int64{threadID}, []int64{3})
	if err != nil {
		t.Fatalf("閲覧数の反映が投稿トランザクションを待った (FOR UPDATE に戻っていないか): %v", err)
	}
	if affected != 1 {
		t.Errorf("反映した行数 = %d, want 1", affected)
	}
}
