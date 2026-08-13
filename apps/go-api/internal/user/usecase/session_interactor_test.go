package usecase

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/user/domain/model"
)

// ---------------------------------------------------------------------------
// セッション検証
// ---------------------------------------------------------------------------
//
// フェイクは interactor_test.go と共有しています (同じパッケージ)。
// **依存として渡すのは sessions だけ**である点がこのファイルの要点になります
// —— users も provider も要らないことが、型として現れている必要があります
// (docs/adr/0005-authentication.md 決定 4)。

func TestAuthenticate(t *testing.T) {
	t.Parallel()

	const token = model.SessionToken("live-token")
	owner := model.SessionOwner{
		ID:          42,
		PublicID:    uuid.MustParse("01920000-0000-7000-8000-000000000001"),
		Email:       "h@example.com",
		DisplayName: "ホシノ",
	}
	uc := NewSessionInteractor(&fakeSessionRepo{liveToken: token, owner: owner}, nil)

	got, err := uc.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate が失敗した: %v", err)
	}
	if got.Me.PublicID != owner.PublicID {
		t.Errorf("PublicID = %v, want %v", got.Me.PublicID, owner.PublicID)
	}
	if got.Me.DisplayName != "ホシノ" {
		t.Errorf("DisplayName = %q", got.Me.DisplayName)
	}
	// **内部 ID も返る。** 投稿者の紐付け (author_id) に要る値で、
	// これが欠けると匿名投稿しかできなくなる。
	if got.UserID != owner.ID {
		t.Errorf("UserID = %d, want %d", got.UserID, owner.ID)
	}
}

// **見つからないセッションは 404 ではなく 401。**
//
// セッションの有無は認証の問題であり、リソースの有無ではない。
// ここを ErrNotFound のまま通すと、未ログインが 404 で返る。
func TestAuthenticate_NotFoundBecomesUnauthenticated(t *testing.T) {
	t.Parallel()

	uc := NewSessionInteractor(&fakeSessionRepo{liveToken: "other"}, nil)

	_, err := uc.Authenticate(context.Background(), "無効なトークン")
	if !errors.Is(err, apperr.ErrUnauthenticated) {
		t.Fatalf("err = %v, want apperr.ErrUnauthenticated", err)
	}
	if errors.Is(err, apperr.ErrNotFound) {
		t.Error("ErrNotFound のまま漏れている (HTTP 層で 404 になる)")
	}
}

func TestAuthenticate_EmptyTokenIsUnauthenticated(t *testing.T) {
	t.Parallel()

	uc := NewSessionInteractor(&fakeSessionRepo{liveToken: "x"}, nil)

	if _, err := uc.Authenticate(context.Background(), ""); !errors.Is(err, apperr.ErrUnauthenticated) {
		t.Fatalf("err = %v, want apperr.ErrUnauthenticated", err)
	}
}

// DB 由来のエラーは 401 に化けさせない。500 のまま上げる。
// ここを握りつぶすと、DB 障害が「未ログイン」として見えてしまう。
func TestAuthenticate_PropagatesOtherErrors(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("DB がダウンしています")
	uc := NewSessionInteractor(&fakeSessionRepo{findErr: sentinel}, nil)

	_, err := uc.Authenticate(context.Background(), "t")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if errors.Is(err, apperr.ErrUnauthenticated) {
		t.Error("DB 障害が未ログインとして扱われている")
	}
}

// ロールがセッションの検証結果から運ばれること。
// 毎リクエストの権限判定がここに依存する。
func TestAuthenticate_CarriesRole(t *testing.T) {
	t.Parallel()

	const token = model.SessionToken("t")
	owner := model.SessionOwner{
		ID: 42, PublicID: uuid.MustParse("01920000-0000-7000-8000-000000000001"),
		Email: "h@example.com", DisplayName: "ホシノ", Role: model.RoleModerator,
	}
	uc := NewSessionInteractor(&fakeSessionRepo{liveToken: token, owner: owner}, nil)

	got, err := uc.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate が失敗した: %v", err)
	}
	if got.Role != model.RoleModerator {
		t.Errorf("Role = %q, want moderator", got.Role)
	}
	// 自分のロールは Me にも載る (フロントが導線を出し分けるため)。
	if got.Me.Role != model.RoleModerator {
		t.Errorf("Me.Role = %q, want moderator", got.Me.Role)
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()

	sessions := &fakeSessionRepo{}
	uc := NewSessionInteractor(sessions, nil)

	if err := uc.Logout(context.Background(), "token-1"); err != nil {
		t.Fatalf("Logout が失敗した: %v", err)
	}
	if len(sessions.deleted) != 1 || sessions.deleted[0] != "token-1" {
		t.Errorf("削除されたトークン = %v", sessions.deleted)
	}
}

// ---------------------------------------------------------------------------
// プロフィール画像 (docs/adr/0007-image-storage.md)
// ---------------------------------------------------------------------------

// fakeImageResolver は所有権の判定を差し替えられるフェイクです。
type fakeImageResolver struct{ err error }

func (f *fakeImageResolver) EnsureOwned(context.Context, int64, uuid.UUID, string) error {
	return f.err
}
func (f *fakeImageResolver) URL(key string) string { return "https://cdn.test/" + key }

// **アップロードした画像があればそちらを返す** (ADR 0007 のスキーマ)。
// 無ければ Google のものを返す。クライアントは 1 つのフィールドだけを見る。
func TestAuthenticate_PrefersUploadedAvatar(t *testing.T) {
	t.Parallel()

	const token = model.SessionToken("t")
	googleURL := "https://lh3.googleusercontent.com/a/xxx"
	key := "images/018f2c00-0000-7000-8000-000000000001.webp"

	owner := model.SessionOwner{
		ID: 42, PublicID: uuid.New(), Email: "h@example.com",
		DisplayName: "ホシノ", AvatarURL: &googleURL, AvatarObjectKey: &key,
	}
	uc := NewSessionInteractor(
		&fakeSessionRepo{liveToken: token, owner: owner}, &fakeImageResolver{})

	got, err := uc.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate が失敗した: %v", err)
	}
	if got.Me.AvatarURL == nil {
		t.Fatal("AvatarURL が nil")
	}
	if *got.Me.AvatarURL != "https://cdn.test/"+key {
		t.Errorf("AvatarURL = %q, want アップロードした画像の URL", *got.Me.AvatarURL)
	}
}

// アップロードしていなければ Google のものを返すこと。
func TestAuthenticate_FallsBackToGoogleAvatar(t *testing.T) {
	t.Parallel()

	const token = model.SessionToken("t")
	googleURL := "https://lh3.googleusercontent.com/a/xxx"

	owner := model.SessionOwner{
		ID: 42, PublicID: uuid.New(), Email: "h@example.com",
		DisplayName: "ホシノ", AvatarURL: &googleURL,
	}
	uc := NewSessionInteractor(
		&fakeSessionRepo{liveToken: token, owner: owner}, &fakeImageResolver{})

	got, err := uc.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate が失敗した: %v", err)
	}
	if got.Me.AvatarURL == nil || *got.Me.AvatarURL != googleURL {
		t.Errorf("AvatarURL = %v, want Google のもの", got.Me.AvatarURL)
	}
}

func TestSetAvatar(t *testing.T) {
	t.Parallel()

	owner := model.SessionOwner{
		ID: 42, PublicID: uuid.New(), Email: "h@example.com", DisplayName: "ホシノ",
	}
	imageID := uuid.New()

	t.Run("設定すると URL が入る", func(t *testing.T) {
		t.Parallel()

		uc := NewSessionInteractor(&fakeSessionRepo{owner: owner}, &fakeImageResolver{})

		me, err := uc.SetAvatar(context.Background(), 42, &imageID)
		if err != nil {
			t.Fatalf("SetAvatar が失敗した: %v", err)
		}
		if me.AvatarURL == nil {
			t.Fatal("AvatarURL が nil")
		}
	})

	t.Run("nil で解除できる", func(t *testing.T) {
		t.Parallel()

		uc := NewSessionInteractor(&fakeSessionRepo{owner: owner}, &fakeImageResolver{})

		me, err := uc.SetAvatar(context.Background(), 42, nil)
		if err != nil {
			t.Fatalf("SetAvatar が失敗した: %v", err)
		}
		// 解除するとフェイクは AvatarObjectKey を付けないので、
		// Google のもの (このフェイクでは nil) に戻る。
		if me.AvatarURL != nil {
			t.Errorf("AvatarURL = %v, want nil (解除された)", *me.AvatarURL)
		}
	})

	// **他人の画像は 404。** 403 だと「その ID の画像が存在すること」が漏れる。
	t.Run("他人の画像は拒否される", func(t *testing.T) {
		t.Parallel()

		uc := NewSessionInteractor(&fakeSessionRepo{owner: owner},
			&fakeImageResolver{err: fmt.Errorf("他人の画像です: %w", apperr.ErrNotFound)})

		if _, err := uc.SetAvatar(context.Background(), 42, &imageID); !errors.Is(err, apperr.ErrNotFound) {
			t.Errorf("err = %v, want apperr.ErrNotFound", err)
		}
	})

	// ストレージが未設定の環境では 503。**黙って設定しない。**
	t.Run("ストレージが未設定なら 503", func(t *testing.T) {
		t.Parallel()

		uc := NewSessionInteractor(&fakeSessionRepo{owner: owner}, nil)

		if _, err := uc.SetAvatar(context.Background(), 42, &imageID); !errors.Is(err, apperr.ErrUnavailable) {
			t.Errorf("err = %v, want apperr.ErrUnavailable", err)
		}
	})

	// 解除はストレージが未設定でもできること (画像を触らないため)。
	t.Run("解除はストレージが未設定でもできる", func(t *testing.T) {
		t.Parallel()

		uc := NewSessionInteractor(&fakeSessionRepo{owner: owner}, nil)

		if _, err := uc.SetAvatar(context.Background(), 42, nil); err != nil {
			t.Errorf("解除が失敗した: %v", err)
		}
	})
}
