package usecase

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/moderation/domain/model"
	"develop-experiments/apps/go-api/internal/moderation/domain/repository"
)

// ---------------------------------------------------------------------------
// フェイク
// ---------------------------------------------------------------------------

// fakeRepo は「何が呼ばれたか」を記録します。
//
// **削除は既定で成功します。** 見たいのはインタラクタ側の判断
// (順序・解釈・正規化) なので、対象の生存はここでは再現しません。
type fakeRepo struct {
	// touched は削除系メソッドが 1 度でも呼ばれたかです。
	// **権限の検査が先に行われること**を見るために使います。
	touched bool

	deletedThreadID  *int64
	deletedCommentTh *int64
	deletedCommentID *int64
	deletedImageID   *uuid.UUID

	recorded  *model.Action
	deleteErr error

	// ロール変更で渡された値。**何が書かれたか**を見るために持ちます。
	changedTo       *string
	changedPublicID *uuid.UUID
	changeErr       error
}

var _ repository.Repository = (*fakeRepo)(nil)

func (f *fakeRepo) WithinTx(_ context.Context, fn func(repository.Repository) error) error {
	return fn(f)
}

func (f *fakeRepo) RecordAction(_ context.Context, a *model.Action) (*model.Action, error) {
	saved := *a
	saved.ID = 1
	saved.CreatedAt = time.Unix(1_700_000_000, 0).UTC()
	f.recorded = &saved
	return &saved, nil
}

func (f *fakeRepo) SoftDeleteThread(_ context.Context, id int64) error {
	f.touched = true
	f.deletedThreadID = &id
	return f.deleteErr
}

func (f *fakeRepo) SoftDeleteComment(_ context.Context, threadID, id int64) error {
	f.touched = true
	f.deletedCommentTh, f.deletedCommentID = &threadID, &id
	return f.deleteErr
}

func (f *fakeRepo) MarkImageDeleted(_ context.Context, id uuid.UUID) error {
	f.touched = true
	f.deletedImageID = &id
	return f.deleteErr
}

// ChangeRole は呼ばれた引数を記録します。
func (f *fakeRepo) ChangeRole(_ context.Context, publicID uuid.UUID, role string) (int64, error) {
	f.changedTo = &role
	f.changedPublicID = &publicID
	return 4242, f.changeErr
}

func moderator() model.Actor { return model.Actor{UserID: 42, CanModerate: true} }

// admin は「権限を配れる」実行者です。
func admin() model.Actor {
	return model.Actor{UserID: 42, CanModerate: true, CanChangeRoles: true}
}

// ---------------------------------------------------------------------------
// 検査
// ---------------------------------------------------------------------------

// **権限の検査が、対象に触る前に行われること。**
//
// 逆にすると、権限の無い利用者が 403 と 404 の差で
// 「その ID の対象が存在すること」を確かめられます。
// 総当たりで存在する ID を列挙できるため、順序そのものが防御になります。
func TestDelete_ChecksPermissionBeforeTouchingTarget(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	_, err := NewInteractor(repo).Delete(context.Background(), DeleteCommand{
		Actor:    model.Actor{UserID: 1, CanModerate: false},
		Action:   model.ActionDeleteThread,
		TargetID: "7",
	})
	if !errors.Is(err, apperr.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if repo.touched {
		t.Error("権限が無いのに対象へ触っている")
	}
	if repo.recorded != nil {
		t.Error("権限が無いのに記録が書かれている")
	}
}

// **権限があっても、削除以外は受け付けないこと。**
func TestDelete_RejectsNonDeleteAction(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	_, err := NewInteractor(repo).Delete(context.Background(), DeleteCommand{
		Actor:    moderator(),
		Action:   model.ActionChangeRole,
		TargetID: "1",
	})
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
	if repo.touched {
		t.Error("削除以外の操作で対象へ触っている")
	}
}

// **記録に残る target_id が正規化されていること。**
//
// 入力をそのまま書くと、同じ対象への操作が "007" と "7"、
// 大文字と小文字の UUID のように複数の表記で残ります。
// moderation_actions_target_idx を引いても片方しか出てこなくなります。
func TestDelete_NormalizesTargetID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		action model.ActionType
		in     string
		want   string
	}{
		{"先頭の 0 を落とす", model.ActionDeleteThread, "007", "7"},
		{
			"UUID を小文字に揃える",
			model.ActionDeleteImage,
			"018F2C00-0000-7000-8000-000000000001",
			"018f2c00-0000-7000-8000-000000000001",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := &fakeRepo{}
			cmd := DeleteCommand{Actor: moderator(), Action: tc.action, TargetID: tc.in}
			if tc.action == model.ActionDeleteComment {
				th := int64(1)
				cmd.ThreadID = &th
			}
			got, err := NewInteractor(repo).Delete(context.Background(), cmd)
			if err != nil {
				t.Fatalf("Delete が失敗した: %v", err)
			}
			if got.TargetID != tc.want {
				t.Errorf("target_id = %q, want %q", got.TargetID, tc.want)
			}
		})
	}
}

// **コメントの削除にスレッド ID が要ること。**
//
// 無いまま進めると 8 パーティションすべてを走査する UPDATE になります。
// 0 や負数も弾きます (IDENTITY は 1 から始まる)。
func TestDelete_CommentRequiresValidThreadID(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	valid := int64(1)

	tests := []struct {
		name     string
		threadID *int64
		wantErr  bool
	}{
		{"未指定は弾く", nil, true},
		{"0 は弾く", &zero, true},
		{"1 は通る", &valid, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := &fakeRepo{}
			_, err := NewInteractor(repo).Delete(context.Background(), DeleteCommand{
				Actor:    moderator(),
				Action:   model.ActionDeleteComment,
				TargetID: "10",
				ThreadID: tc.threadID,
			})
			if tc.wantErr {
				if !errors.Is(err, apperr.ErrInvalidArgument) {
					t.Fatalf("err = %v, want ErrInvalidArgument", err)
				}
				if repo.touched {
					t.Error("弾いたのに対象へ触っている")
				}
				return
			}
			if err != nil {
				t.Fatalf("Delete が失敗した: %v", err)
			}
			// **パーティションキーがそのまま渡ること。**
			if repo.deletedCommentTh == nil || *repo.deletedCommentTh != 1 {
				t.Errorf("thread_id = %v, want 1", repo.deletedCommentTh)
			}
			if repo.deletedCommentID == nil || *repo.deletedCommentID != 10 {
				t.Errorf("comment id = %v, want 10", repo.deletedCommentID)
			}
		})
	}
}

// **理由の正規化。**
//
// 入力欄が「未入力」を空文字で送ってくるため、そのまま保存すると
// 「理由あり」なのに中身が無い記録になります。
func TestDelete_NormalizesReason(t *testing.T) {
	t.Parallel()

	ptr := func(s string) *string { return &s }

	tests := []struct {
		name string
		in   *string
		want *string
	}{
		{"nil はそのまま", nil, nil},
		{"空文字は nil", ptr(""), nil},
		{"空白だけは nil", ptr("   \n\t "), nil},
		{"前後の空白を落とす", ptr("  荒らし  "), ptr("荒らし")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := &fakeRepo{}
			got, err := NewInteractor(repo).Delete(context.Background(), DeleteCommand{
				Actor: moderator(), Action: model.ActionDeleteThread,
				TargetID: "7", Reason: tc.in,
			})
			if err != nil {
				t.Fatalf("Delete が失敗した: %v", err)
			}
			switch {
			case tc.want == nil && got.Reason != nil:
				t.Errorf("reason = %q, want nil", *got.Reason)
			case tc.want != nil && got.Reason == nil:
				t.Errorf("reason = nil, want %q", *tc.want)
			case tc.want != nil && *got.Reason != *tc.want:
				t.Errorf("reason = %q, want %q", *got.Reason, *tc.want)
			}
		})
	}
}

// **理由の長さは文字数で数えること。**
//
// バイト数で数えると、日本語の理由が 1/3 の長さで弾かれます。
// DB の CHECK 制約は char_length なので、そちらと同じ数え方に揃えます。
func TestDelete_ReasonLengthCountsRunes(t *testing.T) {
	t.Parallel()

	// 上限ちょうどの日本語 (バイト数では 3 倍になる)。
	ok := strings.Repeat("あ", maxReasonLength)
	if _, err := NewInteractor(&fakeRepo{}).Delete(context.Background(), DeleteCommand{
		Actor: moderator(), Action: model.ActionDeleteThread, TargetID: "7", Reason: &ok,
	}); err != nil {
		t.Fatalf("上限ちょうどの日本語が弾かれた: %v", err)
	}

	over := strings.Repeat("あ", maxReasonLength+1)
	_, err := NewInteractor(&fakeRepo{}).Delete(context.Background(), DeleteCommand{
		Actor: moderator(), Action: model.ActionDeleteThread, TargetID: "7", Reason: &over,
	})
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}

// **削除が失敗したら記録も残らないこと。**
func TestDelete_NoRecordWhenDeleteFails(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{deleteErr: apperr.ErrNotFound}
	_, err := NewInteractor(repo).Delete(context.Background(), DeleteCommand{
		Actor: moderator(), Action: model.ActionDeleteThread, TargetID: "7",
	})
	if !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if repo.recorded != nil {
		t.Error("削除に失敗したのに記録が書かれている")
	}
}

// **理由の上限が 3 か所で揃っていること。**
//
//	この定数 / DB の CHECK 制約 / 仕様書の maxLength
//
// ずれると、仕様書を通ったリクエストが DB の制約で 500 になるか、
// 逆にアプリが弾く長さを DB が受け入れる形になります。
func TestMaxReasonLength_AgreesAcrossSources(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "..", "..", "..")

	readFile := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s を読めない: %v", rel, err)
		}
		return string(b)
	}

	migration := readFile("apps/go-api/db/migrations/000008_add_moderation.up.sql")
	m := regexp.MustCompile(`char_length\(reason\) <= (\d+)`).FindStringSubmatch(migration)
	if m == nil {
		t.Fatal("000008 から reason の上限を読み取れなかった")
	}
	if got, _ := strconv.Atoi(m[1]); got != maxReasonLength {
		t.Errorf("DB の CHECK 制約 = %d, Go = %d", got, maxReasonLength)
	}

	spec := readFile("api/openapi.yaml")
	// CreateModerationActionRequest の reason の maxLength を拾う。
	block := regexp.MustCompile(`(?ms)^        reason:\n          type: string\n          maxLength: (\d+)`).
		FindStringSubmatch(spec)
	if block == nil {
		t.Fatal("openapi.yaml から reason の maxLength を読み取れなかった")
	}
	if got, _ := strconv.Atoi(block[1]); got != maxReasonLength {
		t.Errorf("仕様書の maxLength = %d, Go = %d", got, maxReasonLength)
	}
}
