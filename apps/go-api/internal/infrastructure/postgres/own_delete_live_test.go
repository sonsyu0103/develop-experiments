package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/apperr"
	commentmodel "develop-experiments/apps/go-api/internal/comment/domain/model"
)

// ---------------------------------------------------------------------------
// 実 DB に対する検証: 本人による削除 (ADR 0005 の権限モデル / ADR 0003 未決 #7)
// ---------------------------------------------------------------------------
//
// **フェイクでは測れないものが 2 つあります。**
//
//   - 三値論理。匿名投稿は author_id が NULL なので `author_id = $2` は
//     NULL になり、真になりません。**Go 側で書くと nil 比較になってしまい、
//     SQL の挙動とは別物**になります
//   - パーティションキー。主キーが (thread_id, id) なので、
//     thread_id が違えば同じ id でも当たりません
//
// 403 と 404 の撃ち分けも、2 本のクエリの組み合わせで決まるため、
// ここでしか通りません。

// seedCommentBy は投稿者を指定してコメントを 1 件作ります。
//
// seedComment (匿名固定) と分けているのは、所有の検査には
// **author_id が入った行と NULL の行の両方**が要るためです。
func seedCommentBy(t *testing.T, pool *pgxpool.Pool, threadID int64, authorID *int64, seq int32) int64 {
	t.Helper()

	var id int64
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO comments (thread_id, seq, author_name, body, author_id)
		VALUES ($1, $2, '名無しさん', 'live: 本人削除の検証', $3)
		RETURNING id`, threadID, seq, authorID).Scan(&id); err != nil {
		t.Fatalf("コメントを作れませんでした: %v", err)
	}
	return id
}

// threadAlive はスレッドが生きているかを返します。
func threadAlive(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()

	var alive bool
	if err := pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM threads WHERE id = $1 AND deleted_at IS NULL)`,
		id).Scan(&alive); err != nil {
		t.Fatalf("スレッドを読めませんでした: %v", err)
	}
	return alive
}

// **自分のスレッドだけが消せること。**
//
// 他人のものと匿名のものは ErrPermissionDenied、
// 無いものと削除済みのものは ErrNotFound になります。
func TestThreadRepository_SoftDeleteOwn_Live(t *testing.T) {
	pool := liveDB(t)
	const (
		ownerID = int64(900500)
		otherID = int64(900501)
	)
	seedOwner(t, pool, ownerID)
	seedOwner(t, pool, otherID)

	repo := NewThreadRepository(pool)
	owner := ownerID

	t.Run("自分のものは消せる", func(t *testing.T) {
		id := seedThread(t, pool, &owner)
		if err := repo.SoftDeleteOwn(t.Context(), id, ownerID); err != nil {
			t.Fatalf("SoftDeleteOwn が失敗した: %v", err)
		}
		if threadAlive(t, pool, id) {
			t.Error("論理削除されていない")
		}
		// 2 回目は 404。**403 にしない** ——
		// 自分のものだと分かっているのに消せないのは「既に消えている」だけ。
		if err := repo.SoftDeleteOwn(t.Context(), id, ownerID); !errors.Is(err, apperr.ErrNotFound) {
			t.Errorf("2 回目の err = %v, want ErrNotFound", err)
		}
	})

	t.Run("他人のものは消せない", func(t *testing.T) {
		other := otherID
		id := seedThread(t, pool, &other)
		err := repo.SoftDeleteOwn(t.Context(), id, ownerID)
		if !errors.Is(err, apperr.ErrPermissionDenied) {
			t.Fatalf("err = %v, want ErrPermissionDenied", err)
		}
		if !threadAlive(t, pool, id) {
			t.Error("他人のスレッドが消えている")
		}
	})

	// **三値論理がそのまま効いていること。**
	// author_id が NULL のとき `author_id = $2` は NULL になり、
	// 真にならないので当たりません。明示的な IS NOT NULL は書いていません。
	t.Run("匿名のものは本人でも消せない", func(t *testing.T) {
		id := seedThread(t, pool, nil)
		err := repo.SoftDeleteOwn(t.Context(), id, ownerID)
		if !errors.Is(err, apperr.ErrPermissionDenied) {
			t.Fatalf("err = %v, want ErrPermissionDenied", err)
		}
		if !threadAlive(t, pool, id) {
			t.Error("匿名スレッドが消えている")
		}
	})

	t.Run("無いものは 404", func(t *testing.T) {
		if err := repo.SoftDeleteOwn(t.Context(), 999_999_999, ownerID); !errors.Is(err, apperr.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

// **自分のコメントだけが消せること。**
//
// スレッドの検査に加えて、**パーティションキーが効いていること**を見ます。
func TestCommentRepository_SoftDeleteOwn_Live(t *testing.T) {
	pool := liveDB(t)
	const (
		ownerID = int64(900502)
		otherID = int64(900503)
	)
	seedOwner(t, pool, ownerID)
	seedOwner(t, pool, otherID)

	owner, other := ownerID, otherID
	threadID := seedThread(t, pool, &owner)
	otherThreadID := seedThread(t, pool, &owner)

	repo := NewCommentRepository(pool, "")

	// **消すのは最大のレス番号にする。** 途中の番号を消しても
	// MAX(seq) が動かないため、「削除された番号を再利用しない」を
	// 検査できない (レビュー指摘と同じ形の穴になる)。
	theirs := seedCommentBy(t, pool, threadID, &other, 1)
	anon := seedCommentBy(t, pool, threadID, nil, 2)
	mine := seedCommentBy(t, pool, threadID, &owner, 3)

	alive := func(id int64) bool {
		var ok bool
		if err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM comments WHERE thread_id = $1 AND id = $2 AND deleted_at IS NULL)`,
			threadID, id).Scan(&ok); err != nil {
			t.Fatalf("コメントを読めませんでした: %v", err)
		}
		return ok
	}

	// **スレッド ID が違えば当たらない。** ここが成功するなら、
	// クエリから thread_id が落ちて 8 パーティションを走査している。
	if err := repo.SoftDeleteOwn(t.Context(), otherThreadID, mine, ownerID); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("別スレッドの ID で消せてしまった (err=%v)", err)
	}
	if !alive(mine) {
		t.Fatal("別スレッド指定で消えている")
	}

	// **失敗の分類も、対象と同じパーティションから決まること。**
	//
	// 上の検査は「自分のコメント」を別スレッド指定で消そうとするので、
	// 所有の問い合わせから thread_id が落ちても**同じ 404 に着地する**
	// (自分のものが見つかり、削除との競合として 404 になる)。
	// 変異プローブで実測して分かった穴なので、他人のコメントで見る ——
	// thread_id が落ちていると、無関係な行が見つかって 403 になる。
	if err := repo.SoftDeleteOwn(t.Context(), otherThreadID, theirs, ownerID); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("別スレッド指定の他人のコメント err = %v, want ErrNotFound "+
			"(所有の問い合わせから thread_id が落ちている)", err)
	}

	if err := repo.SoftDeleteOwn(t.Context(), threadID, mine, ownerID); err != nil {
		t.Fatalf("自分のコメントを消せなかった: %v", err)
	}
	if alive(mine) {
		t.Error("論理削除されていない")
	}
	if err := repo.SoftDeleteOwn(t.Context(), threadID, mine, ownerID); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("2 回目の err = %v, want ErrNotFound", err)
	}

	if err := repo.SoftDeleteOwn(t.Context(), threadID, theirs, ownerID); !errors.Is(err, apperr.ErrPermissionDenied) {
		t.Errorf("他人のコメント err = %v, want ErrPermissionDenied", err)
	}
	if err := repo.SoftDeleteOwn(t.Context(), threadID, anon, ownerID); !errors.Is(err, apperr.ErrPermissionDenied) {
		t.Errorf("匿名のコメント err = %v, want ErrPermissionDenied", err)
	}
	if !alive(theirs) || !alive(anon) {
		t.Error("権限が無いのにコメントが消えている")
	}

	// **レス番号は空いたまま** (ADR 0019 決定 5)。
	// 再利用すると、過去の >>1 が別の投稿を指すようになる。
	//
	// **採番経路を実際に通すこと** (レビュー指摘)。
	// 初版は seedCommentBy で seq を明示指定して INSERT していたため、
	// 「1 にならないこと」がどんな実装でも真になり、
	// **決定 5 が壊れても緑のままだった。**
	// seq を決めるのは CreateCommentAutoSeq なので、そちらを通す。
	created, err := repo.Create(t.Context(), commentmodel.Reconstruct(
		0, threadID, 0, "名無しさん", nil, nil, "live: 欠番の確認", time.Time{}))
	if err != nil {
		t.Fatalf("採番経路でコメントを作れませんでした: %v", err)
	}
	// **消したのは最大の seq = 3。** 生きているのは 1 と 2 だけになる。
	//   再利用しない (正しい) : MAX(seq) = 3 -> 4
	//   再利用する   (誤り)   : 生存だけを見て MAX = 2 -> 3
	// この差が出るのは、消した番号が最大のときだけになる。
	if created.Seq != 4 {
		t.Errorf("採番された seq = %d, want 4 (削除した番号を再利用している)", created.Seq)
	}
}
