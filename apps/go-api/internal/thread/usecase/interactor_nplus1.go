package usecase

import (
	"context"

	"golang.org/x/sync/errgroup"

	"develop-experiments/apps/go-api/internal/pagination"
	"develop-experiments/apps/go-api/internal/thread/domain/model"
	"develop-experiments/apps/go-api/internal/thread/domain/repository"
)

// DefaultAggregationConcurrency は N+1 版の並列度の既定値です。
//
// この値を DB 接続プールの最大接続数より大きくしても意味がありません。
// goroutine はプールから接続を借りられず待たされるだけで、
// 「並列度を上げたのに速くならない」という結果になります。
// ベンチマークではこの点も併せて計測します。
const DefaultAggregationConcurrency = 8

// NPlusOneInteractor は「スレッドを引いたあと、スレッドごとに
// コメント数と投稿者を引き、それを goroutine で並列化する」実装です。
//
// 【この実装は本番経路では使いません】
// Phase 4 のベンチマークで、単一クエリ版 (ThreadInteractor.FetchThreadList) と
// 比較するために意図的に N+1 を再現しています。
//
// 並列化しても DB へのラウンドトリップ回数は 2N+1 回のままなので、
// レイテンシは「最も遅いクエリ + プール待ち」に律速されます。
// 一方で DB 側の負荷は N 倍になるため、同時接続が増えるほど不利になります。
//
// **投稿者もここで解決します。** 単一クエリ版は LEFT JOIN users で
// 投稿者まで返すため、こちらが引かないと「同じ仕事をしていない 2 つ」を
// 比べることになります (ADR 0014「測定の前提が 1 つ崩れている」)。
// 往復が 1 スレッドあたり 2 回になるのは N+1 の性質そのもので、
// **不利に見えるのは再現が正しいから**になります。
type NPlusOneInteractor struct {
	repo        repository.BenchmarkRepository
	concurrency int
}

// NewNPlusOneInteractor はベンチマーク用インタラクターを生成します。
// concurrency が 0 以下の場合は DefaultAggregationConcurrency を使います。
func NewNPlusOneInteractor(repo repository.BenchmarkRepository, concurrency int) *NPlusOneInteractor {
	if concurrency <= 0 {
		concurrency = DefaultAggregationConcurrency
	}
	return &NPlusOneInteractor{repo: repo, concurrency: concurrency}
}

// FetchThreadListNPlusOne はスレッド一覧を N+1 クエリ + 並列集計で取得します。
func (i *NPlusOneInteractor) FetchThreadListNPlusOne(
	ctx context.Context, page pagination.Page,
) (ThreadListResult, error) {
	threads, err := i.repo.ListThreadsOnly(ctx, page)
	if err != nil {
		return ThreadListResult{}, err
	}

	summaries := make([]model.Summary, len(threads))

	// errgroup.WithContext は、いずれかの goroutine がエラーを返した時点で
	// ctx をキャンセルする。残りの COUNT クエリも即座に打ち切られるので、
	// 失敗が確定したあとに DB を叩き続けることがない。
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(i.concurrency)

	for idx, t := range threads {
		// 各 goroutine は summaries の異なる添字だけに書き込むため、
		// ミューテックスなしでデータ競合が起きない。
		// この前提は go test -race で検証している。
		g.Go(func() error {
			// errgroup.Go は SetLimit の空きを待つだけで、
			// gctx がキャンセル済みかどうかは見ない。
			// ここで明示的に打ち切らないと、失敗が確定したあとも
			// 残りのタスクが順番に DB を叩き続けてしまう。
			if err := gctx.Err(); err != nil {
				return err
			}

			count, err := i.repo.CountComments(gctx, t.ID)
			if err != nil {
				return err
			}

			// **投稿者も 1 件ずつ引く。** 2 本目の往復を同じ goroutine で
			// 直列に投げるのは、素朴な N+1 実装の形をそのまま再現するため。
			// ここを並列にすると「N+1 を並列化した実装」ではなく
			// 「2N+1 を並列化した実装」になり、比較の対象が変わる。
			author, err := i.repo.FindThreadAuthor(gctx, t.ID)
			if err != nil {
				return err
			}
			t.Author = author

			summaries[idx] = model.Summary{Thread: t, CommentCount: count}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return ThreadListResult{}, err
	}

	// **アイコンの URL は組み立てない。** このインタラクタは Phase 4 の
	// ベンチマーク専用で、本番経路から外れている (ADR 0014)。
	// ImageResolver を持たせると、比較したい N+1 の形に無関係な依存が増える。
	//
	// 投稿者と違って、ここは揃えなくても比較が歪まない ——
	// 単一クエリ版のアイコン解決は LEFT JOIN images 1 つで、
	// ベンチマークのデータセットは icon_image_id が全件 NULL になっている。
	// **揃えるべきなのは「往復が増える仕事」のほう**になる。
	// 並び順は新着順に固定する。**N+1 の再現が目的**なので、
	// 人気順まで用意すると比較したい形から離れる。
	return (&ThreadInteractor{}).buildListResult(summaries, page.Size, model.ListOrderNew)
}
