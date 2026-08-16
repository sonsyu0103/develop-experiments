package usecase

import (
	"context"
	"strings"
	"testing"
	"time"

	"develop-experiments/apps/go-api/internal/contact/domain/model"
)

// **送れたら sent になること。**
func TestDispatch_Sends(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{pending: []model.Message{pendingMessage(1, 1), pendingMessage(2, 1)}}
	sender := &fakeSender{}

	got, err := NewDispatcher(repo, sender).Dispatch(t.Context())
	if err != nil {
		t.Fatalf("送信に失敗した: %v", err)
	}
	if got.Sent != 2 || got.Total() != 2 {
		t.Errorf("結果 = %+v, want Sent = 2", got)
	}
	if len(repo.sentIDs) != 2 {
		t.Errorf("確定されていない: %v", repo.sentIDs)
	}
	if len(sender.sent) != 2 {
		t.Fatalf("送信件数 = %d, want 2", len(sender.sent))
	}
}

// **失敗しても Dispatch はエラーを返さないこと。**
//
// 返すと、プロバイダが落ちている間じゅうスケジューラが ERROR を
// 吐き続けます (ADR 0010 の 4-3: ERROR は人が対応するものだけ)。
// 失敗は行に記録され、次の周回でやり直されます。
func TestDispatch_SendFailureIsNotAnError(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{pending: []model.Message{pendingMessage(1, 1)}}
	got, err := NewDispatcher(repo, &fakeSender{err: errSMTP}).Dispatch(t.Context())
	if err != nil {
		t.Fatalf("送信の失敗が Dispatch のエラーになった: %v", err)
	}
	if got.Retried != 1 {
		t.Errorf("結果 = %+v, want Retried = 1", got)
	}
	if len(repo.rescheduled) != 1 {
		t.Fatalf("次回へ回されていない: %v", repo.rescheduled)
	}
	if !strings.Contains(repo.rescheduled[0].lastErr, "connection refused") {
		t.Errorf("失敗理由が残っていない: %q", repo.rescheduled[0].lastErr)
	}
}

// **試行回数の上限を超えたら打ち切ること** (ADR 0008 決定 1)。
//
// 無限にリトライすると、送信できないメールが延々とプロバイダを叩き続けます。
func TestDispatch_GivesUpAtMaxAttempts(t *testing.T) {
	t.Parallel()

	// 確保の時点で attempt_count が増えているので、
	// ここに来る値は「今回を含めた試行回数」になる。
	repo := &fakeRepo{pending: []model.Message{pendingMessage(7, 3)}}
	got, err := NewDispatcher(repo, &fakeSender{err: errSMTP},
		WithMaxAttempts(3)).Dispatch(t.Context())
	if err != nil {
		t.Fatalf("Dispatch がエラーになった: %v", err)
	}
	if got.Failed != 1 {
		t.Errorf("結果 = %+v, want Failed = 1", got)
	}
	if len(repo.failed) != 1 || repo.failed[0] != 7 {
		t.Errorf("打ち切られていない: %v", repo.failed)
	}
	if len(repo.rescheduled) != 0 {
		t.Error("打ち切ったのに次回へも回されている")
	}
}

// **バックオフが指数で伸び、上限で頭打ちになること。**
func TestDispatcher_Backoff(t *testing.T) {
	t.Parallel()

	d := NewDispatcher(&fakeRepo{}, &fakeSender{},
		WithBackoff(1*time.Minute, 8*time.Minute))

	want := map[int32]time.Duration{
		1: 1 * time.Minute,
		2: 2 * time.Minute,
		3: 4 * time.Minute,
		4: 8 * time.Minute,
		// 上限で頭打ち。
		5: 8 * time.Minute,
		9: 8 * time.Minute,
	}
	for attempt, expected := range want {
		if got := d.backoff(attempt); got != expected {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, expected)
		}
	}
}

// **中断されたら、そこで止まること。**
//
// 確保済みの行はリースが切れれば拾い直されるので失われません。
// 逆に、シャットダウン中に残り全件の SMTP を待つと猶予を使い切ります。
func TestDispatch_StopsOnCancel(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{pending: []model.Message{
		pendingMessage(1, 1), pendingMessage(2, 1), pendingMessage(3, 1),
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, err := NewDispatcher(repo, &fakeSender{}).Dispatch(ctx)
	if err != nil {
		t.Fatalf("Dispatch がエラーになった: %v", err)
	}
	if got.Total() != 0 {
		t.Errorf("中断後も処理された: %+v", got)
	}
}

// **本文にラベルつきで入力が並ぶこと。ヘッダは 1 つも含まれないこと。**
//
// ADR 0008 決定 3 の「ユーザー入力は本文にのみ入れる」を、
// 組み立てた文字列の側から確かめます。
func TestComposeBody(t *testing.T) {
	t.Parallel()

	userID := int64(42)
	body := composeBody(model.Message{
		ID:        7,
		UserID:    &userID,
		Name:      "ホシノ",
		Email:     "hoshino@example.com",
		Subject:   "ログインできません",
		Body:      "画面が戻ってきます。",
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	})

	for _, want := range []string{
		"問い合わせ番号: 7",
		"ログイン: あり (user_id=42)",
		"氏名: ホシノ",
		"メールアドレス (未検証): hoshino@example.com",
		"件名: ログインできません",
		"画面が戻ってきます。",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("本文に %q が無い:\n%s", want, body)
		}
	}

	// **本文はメールのヘッダを作らない。** ここに "Subject:" のような
	// 行頭のヘッダらしき文字列を混ぜないこと ——
	// 実際のヘッダは mail パッケージが組み立てる。
	if strings.HasPrefix(body, "Subject:") || strings.Contains(body, "\nBcc:") {
		t.Errorf("本文がヘッダを含んでいる:\n%s", body)
	}
}

// **匿名の問い合わせがそう分かること。**
//
// 本文中のアドレスは検証されていないので、「名乗っている人」と
// 「ログインしている人」は別です。対応する側が取り違えないよう区別します。
func TestComposeBody_Anonymous(t *testing.T) {
	t.Parallel()

	body := composeBody(model.Message{ID: 1, Name: "名無し", Email: "a@b", Subject: "件名", Body: "本文"})
	if !strings.Contains(body, "ログイン: なし (匿名)") {
		t.Errorf("匿名と分からない:\n%s", body)
	}
}

// **失敗理由がルーン境界で切り詰められること。**
//
// バイト数で切ると、日本語のエラーが途中で壊れて不正な UTF-8 になり、
// DB への保存で落ちます。
func TestTruncate(t *testing.T) {
	t.Parallel()

	if got := truncate("あいうえお", 10); got != "あいうえお" {
		t.Errorf("上限内なのに切られた: %q", got)
	}

	got := truncate(strings.Repeat("あ", 600), maxLastErrorLength)
	if n := len([]rune(got)); n != maxLastErrorLength {
		t.Errorf("切り詰めた長さ = %d 文字, want %d", n, maxLastErrorLength)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("省略記号が付いていない: %q", got[len(got)-10:])
	}
}

// pendingMessage は確保済みの行を作ります。
// attempt は確保の時点で増えたあとの値です。
func pendingMessage(id int64, attempt int32) model.Message {
	return model.Message{
		ID:           id,
		Name:         "ホシノ",
		Email:        "hoshino@example.com",
		Subject:      "件名",
		Body:         "本文",
		AttemptCount: attempt,
		CreatedAt:    time.Unix(1700000000, 0).UTC(),
	}
}
