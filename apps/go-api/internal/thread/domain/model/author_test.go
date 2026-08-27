package model

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// 退会した利用者の投稿は、投稿そのものは残したまま表示だけ差し替える。
//
// **この正規化をここに置くのが要点。** 表示側でやると、
// 一覧・詳細・作成レスポンスのどれか 1 つで必ず漏れる。
func TestNewAuthor_Withdrawn(t *testing.T) {
	t.Parallel()

	deletedAt := time.Unix(1_700_000_000, 0).UTC()
	avatar := "https://example.com/a.png"

	got := NewAuthor(uuid.MustParse("01920000-0000-7000-8000-000000000001"),
		"ホシノ", &avatar, &deletedAt)

	if !got.Withdrawn {
		t.Error("Withdrawn = false, want true")
	}
	if got.DisplayName != WithdrawnDisplayName {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, WithdrawnDisplayName)
	}
	// **公開 ID を返さない。** 返すと、表示側が Withdrawn の分岐を落としたときに
	// 退会した人のマイページへ辿れてしまう。
	if got.PublicID != nil {
		t.Errorf("PublicID = %v, want nil (退会者の識別子は返さない)", got.PublicID)
	}
	if got.AvatarURL != nil {
		t.Errorf("AvatarURL = %v, want nil (退会者の顔写真は残さない)", *got.AvatarURL)
	}
}

// 生存している利用者はそのまま返す。
// 退会側だけを検査すると「常に伏せる」実装でも通ってしまう。
func TestNewAuthor_Active(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("01920000-0000-7000-8000-000000000001")
	avatar := "https://example.com/a.png"

	got := NewAuthor(id, "ホシノ", &avatar, nil)

	if got.Withdrawn {
		t.Error("Withdrawn = true, want false")
	}
	if got.PublicID == nil || *got.PublicID != id {
		t.Errorf("PublicID = %v, want %v", got.PublicID, id)
	}
	if got.DisplayName != "ホシノ" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
	if got.AvatarURL == nil || *got.AvatarURL != avatar {
		t.Errorf("AvatarURL = %v, want %q", got.AvatarURL, avatar)
	}
}
