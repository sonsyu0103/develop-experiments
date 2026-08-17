package usecase

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"develop-experiments/apps/go-api/internal/contact/domain/model"
	"develop-experiments/apps/go-api/internal/contact/domain/repository"
)

// fakeRepo は ContactRepository のフェイクです。
//
// **SKIP LOCKED は再現しません。** それは実 DB でしか確かめられないので、
// postgres パッケージの live テストが受け持ちます
// (contact_repository_live_test.go)。こちらが見るのは
// 「確保した行をどう扱うか」という判断のほうです。
type fakeRepo struct {
	mu sync.Mutex

	// saved は Save に渡された入力です。**正規化済みの値が来ているか**を
	// 呼び出し側から検査するために残します。
	saved []model.Submission
	// recentCount は CountRecent が返す件数です。
	recentCount int64
	// pending は ClaimPending が返す行です。
	pending []model.Message

	sentIDs       []int64
	rescheduled   []rescheduleCall
	failed        []int64
	oldestPending time.Time

	saveErr  error
	countErr error
	claimErr error
}

type rescheduleCall struct {
	id      int64
	backoff time.Duration
	lastErr string
}

var _ repository.ContactRepository = (*fakeRepo)(nil)

func (f *fakeRepo) Save(_ context.Context, s model.Submission) (*model.Message, error) {
	if f.saveErr != nil {
		return nil, f.saveErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, s)

	return &model.Message{
		ID:        int64(len(f.saved)),
		UserID:    s.UserID,
		Name:      s.Name,
		Email:     s.Email,
		Subject:   s.Subject,
		Body:      s.Body,
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}, nil
}

func (f *fakeRepo) CountRecent(_ context.Context, _ netip.Addr, _ time.Duration) (int64, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.recentCount, nil
}

func (f *fakeRepo) ClaimPending(
	_ context.Context, _ time.Duration, batchSize int32,
) ([]model.Message, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// **確保したら消えます。** 実装と同じく、2 回目の呼び出しでは返りません
	// (drain が「0 件になるまで繰り返す」形で呼ぶため、
	//  消えないとテストが止まらなくなります)。
	n := min(int(batchSize), len(f.pending))
	claimed := f.pending[:n]
	f.pending = f.pending[n:]
	return claimed, nil
}

func (f *fakeRepo) MarkSent(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sentIDs = append(f.sentIDs, id)
	return nil
}

func (f *fakeRepo) Reschedule(
	_ context.Context, id int64, backoff time.Duration, lastErr string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescheduled = append(f.rescheduled, rescheduleCall{id: id, backoff: backoff, lastErr: lastErr})
	return nil
}

func (f *fakeRepo) Fail(_ context.Context, id int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, id)
	return nil
}

func (f *fakeRepo) OldestPending(context.Context) (time.Time, bool, error) {
	if f.oldestPending.IsZero() {
		return time.Time{}, false, nil
	}
	return f.oldestPending, true, nil
}

func (f *fakeRepo) ScrubClientIPs(context.Context, time.Duration, int32) (int64, error) {
	return 0, nil
}

// fakeSender は MailSender のフェイクです。
//
// **実際にメールを送るテストは書きません** (ADR 0008)。
// 送信手段の検査は internal/infrastructure/mail 側で、
// 組み立てたバイト列に対して行います。
type fakeSender struct {
	mu   sync.Mutex
	sent []repository.Notification
	// autoReplies は本人へ送った控えです (ADR 0008 決定 2)。
	autoReplies []repository.AutoReply
	// err が非 nil なら常に失敗します。
	err error
	// autoReplyErr が非 nil なら控えの送信だけが失敗します。
	// **運営への通知は成功させます** —— 控えの失敗が問い合わせ本体を
	// 巻き込まないこと (best-effort) を測るために分けてあります。
	autoReplyErr error
}

var _ repository.MailSender = (*fakeSender)(nil)

func (f *fakeSender) Send(_ context.Context, n repository.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, n)
	return nil
}

func (f *fakeSender) SendAutoReply(_ context.Context, r repository.AutoReply) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.autoReplyErr != nil {
		return f.autoReplyErr
	}
	f.autoReplies = append(f.autoReplies, r)
	return nil
}

// errSMTP は送信の失敗を表すテスト用のエラーです。
var errSMTP = errors.New("dial tcp: connection refused")
