package usecase

import (
	"context"
	"develop-experiments/apps/go-api/internal/thread/domain/model"
	"sync"
	"time"
)

// ThreadInteractor は「スレッド一覧を取得して集計する」ユースケースを担当します
type ThreadInteractor struct{}

// ThreadDTO はフロントエンドに返すスレッドのデータ構造です
// @Description スレッドの集計情報
type ThreadDTO struct {
	ID           int    `json:"id" example:"1"`            // スレッドID
	Title        string `json:"title" example:"並列処理を学ぶ部屋"` // スレッドタイトル
	CommentCount int    `json:"commentCount" example:"7"`  // 並列集計されたコメント数
}

// FetchThreadList はスレッド一覧を並列処理で集計して取得します
func (i *ThreadInteractor) FetchThreadList(ctx context.Context) ([]ThreadDTO, error) {

	threads := []*model.Thread{
		model.NewThread(1, "Goの並列処理を学ぶ部屋"),
		model.NewThread(2, "やっぱり君と開発する部屋"),
	}

	var wg sync.WaitGroup
	results := make([]ThreadDTO, len(threads))

	for index, t := range threads {
		wg.Add(1)

		go func(idx int, thread *model.Thread) {
			defer wg.Done()

			// 0.5秒の重い処理のシミュレーション
			time.Sleep(500 * time.Millisecond)

			results[idx] = ThreadDTO{
				ID:           thread.ID,
				Title:        thread.Title,
				CommentCount: (idx + 1) * 7,
			}
		}(index, t)
	}

	wg.Wait()

	return results, nil
}
