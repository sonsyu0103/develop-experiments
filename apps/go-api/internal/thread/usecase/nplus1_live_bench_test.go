package usecase

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/infrastructure/postgres"
	"develop-experiments/apps/go-api/internal/pagination"
)

// ---------------------------------------------------------------------------
// N+1 vs 単一クエリ の実測 (Phase 4)
// ---------------------------------------------------------------------------
//
// **README の Phase 4 で「未測定」として残っていた 1 件。**
// 理由は「比較実装 (interactor_nplus1.go) がユースケース層にあるだけで
// HTTP に繋がっておらず、ベンチから叩く経路が無い」だった。
// ここがその経路になる。
//
// 【HTTP に繋がなかったのはなぜか】
// 繋ぐと、本番のルータに「N+1 で返す口」が生える。設定で塞いだつもりでも、
// 塞ぎ忘れが本番の経路になる形は作りたくない。Go のベンチマークなら
// **実行経路がテストバイナリの中で閉じる**ので、その心配が無い。
//
// 【なぜ EXPLAIN では足りないか】
// N+1 のコストの本体は**往復回数**であり、SQL 1 本の実行時間ではない。
// query-probe.py (EXPLAIN ANALYZE) はサーバ側の実行時間しか見えないので、
// 往復とプール待ちが数字に出ない。ここは Go から実際に往復させて測る。
//
// 使い方:
//
//	make bench-dataset    2,000 スレッド / 20 万コメントを投入する
//	make nplus1-probe     測る
//
// DATABASE_TEST_URL が無ければスキップする (実 DB を使う既存の検査と同じ形)。
// **CI には載せない** —— 数字が環境の性能に左右されるため。

// benchMinThreads はベンチマークに必要な最低スレッド数です。
// 開発シードの 5 件で測っても、1 ページ (20 件) すら埋まりません。
const benchMinThreads = 1000

// benchPool はベンチマーク用の接続プールを返します。未設定ならスキップします。
//
// **MaxConns を明示します。** pgxpool の既定は max(4, GOMAXPROCS) で、
// 測定機の CPU 数によって変わります。N+1 の並列度と接続数の関係が
// この測定の主題の 1 つなので、機械ごとに動く値を既定にはできません。
func benchPool(b *testing.B) *pgxpool.Pool {
	b.Helper()

	dsn := os.Getenv("DATABASE_TEST_URL")
	if dsn == "" {
		b.Skip("DATABASE_TEST_URL が未設定のためスキップ (実 DB が必要)")
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		b.Fatalf("DATABASE_TEST_URL の解析に失敗した: %v", err)
	}
	cfg.MaxConns = benchMaxConns(b)
	cfg.MinConns = cfg.MaxConns // 測定中に接続確立のコストが混ざらないようにする

	pool, err := pgxpool.NewWithConfig(b.Context(), cfg)
	if err != nil {
		b.Fatalf("接続できなかった: %v", err)
	}
	b.Cleanup(pool.Close)

	// **接続を先に張っておく。** MinConns は「維持する下限」であって
	// 「起動時に張る本数」ではない。1 回目の測定だけ接続確立を含むと、
	// 並列度の低い条件が不当に遅く見える。
	warmPool(b, pool, int(cfg.MaxConns))

	if n := countThreads(b, pool); n < benchMinThreads {
		b.Fatalf("スレッドが %d 件しかない。make bench-dataset を先に流すこと "+
			"(開発シードの規模では往復回数の差が出ない)", n)
	}
	return pool
}

// benchMaxConns は接続プールの上限を返します (既定 16)。
func benchMaxConns(b *testing.B) int32 {
	b.Helper()

	raw := strings.TrimSpace(os.Getenv("BENCH_MAX_CONNS"))
	if raw == "" {
		return 16
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		b.Fatalf("BENCH_MAX_CONNS が不正: %q", raw)
	}
	return int32(n)
}

// warmPool は上限まで接続を張ってから返します。
func warmPool(b *testing.B, pool *pgxpool.Pool, n int) {
	b.Helper()

	conns := make([]*pgxpool.Conn, 0, n)
	for range n {
		c, err := pool.Acquire(b.Context())
		if err != nil {
			b.Fatalf("接続の確保に失敗した: %v", err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
}

func countThreads(b *testing.B, pool *pgxpool.Pool) int64 {
	b.Helper()

	var n int64
	if err := pool.QueryRow(b.Context(),
		"SELECT count(*) FROM threads WHERE deleted_at IS NULL").Scan(&n); err != nil {
		b.Fatalf("スレッド数の取得に失敗した: %v", err)
	}
	return n
}

// benchPage はカーソル無しのページを組み立てます。
func benchPage(b *testing.B, size int32) pagination.Page {
	b.Helper()

	page, err := pagination.NewPage(nil, size)
	if err != nil {
		b.Fatalf("ページの組み立てに失敗した: %v", err)
	}
	return page
}

// benchPageSizes は測る件数です。
//
//	20  既定のページサイズ (API の既定値)
//	100 上限 (pagination.MaxSize)。**N+1 の往復が 5 倍になる側**
var benchPageSizes = []int32{20, 100}

// BenchmarkThreadList_SingleQuery は本番経路 (1 往復) を測ります。
//
// 相関サブクエリでコメント数を数え、LEFT JOIN users で投稿者を解決する。
// ImageResolver は nil にする —— N+1 版がアイコンの URL を組み立てないため、
// **同じ仕事に揃える**ほうを優先する (ベンチデータセットは icon_image_id が全件 NULL)。
func BenchmarkThreadList_SingleQuery(b *testing.B) {
	pool := benchPool(b)
	uc := NewThreadInteractor(postgres.NewThreadRepository(pool), nil)

	for _, size := range benchPageSizes {
		page := benchPage(b, size)
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			for b.Loop() {
				if _, err := uc.FetchThreadList(context.Background(), page, nil, nil); err != nil {
					b.Fatalf("FetchThreadList が失敗した: %v", err)
				}
			}
			// 往復回数を出力に載せる。**これが比較の主役**で、
			// ns/op だけ見ても「なぜ遅いか」が読み取れない。
			//
			// **計測ループの後で呼ぶこと。** 先に呼ぶと、
			// ベンチマーク終了時の既定の集計に上書きされて出力に出ない。
			b.ReportMetric(1, "roundtrips/op")
		})
	}
}

// BenchmarkThreadList_NPlusOne は N+1 + goroutine 並列集計を測ります。
//
// 並列度を振るのは、**並列度を上げても接続プールの上限で頭打ちになる**
// ことを数字にするためです (interactor_nplus1.go の
// DefaultAggregationConcurrency のコメントが「ベンチマークで測る」と書いている点)。
func BenchmarkThreadList_NPlusOne(b *testing.B) {
	pool := benchPool(b)
	repo := postgres.NewThreadRepository(pool)

	for _, size := range benchPageSizes {
		page := benchPage(b, size)
		for _, concurrency := range []int{1, 4, 8, 16, 32} {
			uc := NewNPlusOneInteractor(repo, concurrency)
			b.Run(fmt.Sprintf("size=%d/concurrency=%d", size, concurrency), func(b *testing.B) {
				for b.Loop() {
					if _, err := uc.FetchThreadListNPlusOne(context.Background(), page); err != nil {
						b.Fatalf("FetchThreadListNPlusOne が失敗した: %v", err)
					}
				}
				// スレッド 1 件につき COUNT と投稿者の 2 本。加えて一覧の 1 本。
				b.ReportMetric(float64(2*size+1), "roundtrips/op")
			})
		}
	}
}

// TestNPlusOneMatchesSingleQuery は 2 つの実装が同じ結果を返すことを確かめます。
//
// **速さを比べる前に、同じものを作っていることを確かめる。**
// 片方が投稿者を解決していなければ当然そちらが速く、
// その数字は「N+1 のほうが速い」ではなく「仕事をしていない」を意味します
// (ADR 0014「測定の前提が 1 つ崩れている」がまさにこれでした)。
//
// go test の通常経路にも載りますが、DATABASE_TEST_URL が無ければスキップします。
func TestNPlusOneMatchesSingleQuery(t *testing.T) {
	dsn := os.Getenv("DATABASE_TEST_URL")
	if dsn == "" {
		t.Skip("DATABASE_TEST_URL が未設定のためスキップ (実 DB が必要)")
	}

	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("接続できなかった: %v", err)
	}
	t.Cleanup(pool.Close)

	repo := postgres.NewThreadRepository(pool)
	single := NewThreadInteractor(repo, nil)
	nplus1 := NewNPlusOneInteractor(repo, 8)

	page, err := pagination.NewPage(nil, 50)
	if err != nil {
		t.Fatalf("ページの組み立てに失敗した: %v", err)
	}

	wantResult, err := single.FetchThreadList(t.Context(), page, nil, nil)
	if err != nil {
		t.Fatalf("FetchThreadList が失敗した: %v", err)
	}
	gotResult, err := nplus1.FetchThreadListNPlusOne(t.Context(), page)
	if err != nil {
		t.Fatalf("FetchThreadListNPlusOne が失敗した: %v", err)
	}

	if len(gotResult.Threads) != len(wantResult.Threads) {
		t.Fatalf("件数 = %d, want %d", len(gotResult.Threads), len(wantResult.Threads))
	}

	for i := range wantResult.Threads {
		want, got := wantResult.Threads[i], gotResult.Threads[i]
		if got.ID != want.ID {
			t.Fatalf("threads[%d].ID = %d, want %d", i, got.ID, want.ID)
		}
		if got.CommentCount != want.CommentCount {
			t.Errorf("threads[%d] (ID=%d) のコメント数 = %d, want %d",
				i, want.ID, got.CommentCount, want.CommentCount)
		}
		// **投稿者は「いる / いない」まで一致させる。** ここが食い違うと
		// 匿名投稿の分だけ N+1 側の往復が減り、比較が歪みます。
		switch {
		case (got.Author == nil) != (want.Author == nil):
			t.Errorf("threads[%d] (ID=%d) の投稿者の有無が食い違う: N+1=%v, 単一=%v",
				i, want.ID, got.Author != nil, want.Author != nil)
		case got.Author != nil && got.Author.DisplayName != want.Author.DisplayName:
			t.Errorf("threads[%d] (ID=%d) の投稿者名 = %q, want %q",
				i, want.ID, got.Author.DisplayName, want.Author.DisplayName)
		}
	}

	// **閲覧数は 2 つの結果で突き合わせない。**
	// 2 つの実装を別の瞬間に投げているので、その間に閲覧数のフラッシュ
	// (ADR 0006) が挟まると値がずれる。ずれても実装の誤りではないのに、
	// probe 全体が止まってしまう。
	//
	// 確かめたいのは「N+1 側が view_count を引いていること」なので、
	// **0 以外が 1 件でも返っているか**で見る。列を引かなければ全件 0 になる
	// (ベンチデータセットは閲覧数を歪んだ分布で入れてある)。
	if !slices.ContainsFunc(gotResult.Threads, func(d ThreadDTO) bool {
		return d.ViewCount > 0
	}) {
		t.Error("N+1 側の閲覧数が全件 0。view_count を引いていない可能性がある " +
			"(make bench-dataset を流したか確認すること)")
	}
}
