package model

import (
	"testing"
)

func TestNewThread(t *testing.T) {
	id := 42
	title := "並列処理のテスト空間"

	thread := NewThread(id, title)

	if thread.ID != id {
		t.Errorf("Expected ID %d, got %d", id, thread.ID)
	}
	if thread.Title != title {
		t.Errorf("Expected Title %s, got %s", title, thread.Title)
	}
	if thread.CreatedAt.IsZero() {
		t.Error("Expected CreatedAt to be set, but it was zero")
	}
}
