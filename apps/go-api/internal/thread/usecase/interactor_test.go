package usecase

import (
	"context"
	"testing"
)

func TestThreadInteractor_FetchThreadList_RaceCheck(t *testing.T) {
	// インタラクターを初期化（空の構造体）
	interactor := &ThreadInteractor{}

	ctx := context.Background()

	// 並列処理が走るメソッドを呼び出す
	results, err := interactor.FetchThreadList(ctx)
	if err != nil {
		t.Fatalf("Failed to fetch thread list: %v", err)
	}

	// 期待通りの件数が取得できているか
	if len(results) != 2 {
		t.Errorf("Expected 2 threads, got %d", len(results))
	}

	// 各スレッドの集計値の検証
	if results[0].Title != "Goの並列処理を学ぶ部屋" || results[0].CommentCount != 7 {
		t.Errorf("Unexpected result at index 0: %+v", results[0])
	}

	if results[1].Title != "やっぱり君と開発する部屋" || results[1].CommentCount != 14 {
		t.Errorf("Unexpected result at index 1: %+v", results[1])
	}
}
