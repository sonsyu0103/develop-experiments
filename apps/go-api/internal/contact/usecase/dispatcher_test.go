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
	verified := mustVerified(t, "account@example.net")
	body := composeBody(model.Message{
		ID:        7,
		UserID:    &userID,
		Name:      "ホシノ",
		Email:     "hoshino@example.com",
		Subject:   "ログインできません",
		Body:      "画面が戻ってきます。",
		CreatedAt: time.Unix(1700000000, 0).UTC(),
		ReplyTo:   &verified,
	})

	for _, want := range []string{
		"問い合わせ番号: 7",
		"ログイン: あり (user_id=42)",
		"氏名: ホシノ",
		// **2 つのアドレスが両方載ること。** 食い違っていること自体が
		// 対応する人にとっての情報になります (他人のアドレスを入力した
		// 問い合わせが目に見える)。
		"メールアドレス (入力値・未検証): hoshino@example.com",
		"メールアドレス (検証済み・返信先): account@example.net",
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
	// **控えを送っていないことが運営に分かること。** 分からないと、
	// 「控えが届いているはず」という前提で対応が進みます。
	if !strings.Contains(body, "控え: 送っていません") {
		t.Errorf("控えの有無が分からない:\n%s", body)
	}
}

// **匿名の問い合わせに控えを送らないこと** (ADR 0008 決定 2)。
//
// **この 1 件が決定 2 そのものになります。** 入力されたアドレスへ
// 落とすフォールバックを書いた瞬間に落ちます ——
// 他人のアドレスを入力するだけで、このシステムが踏み台になります。
func TestDispatch_NoAutoReplyWithoutVerifiedAddress(t *testing.T) {
	t.Parallel()

	// pendingMessage は ReplyTo を持たない (= 匿名 / 退会済み)。
	repo := &fakeRepo{pending: []model.Message{pendingMessage(1, 1)}}
	sender := &fakeSender{}

	got, err := NewDispatcher(repo, sender).Dispatch(t.Context())
	if err != nil {
		t.Fatalf("Dispatch がエラーになった: %v", err)
	}
	if got.Sent != 1 {
		t.Errorf("結果 = %+v, want Sent = 1", got)
	}
	if len(sender.autoReplies) != 0 {
		t.Fatalf("検証されていないのに控えを送った: %+v", sender.autoReplies)
	}
}

// **検証済みアドレスがあれば控えを送ること。**
func TestDispatch_SendsAutoReplyToVerifiedAddress(t *testing.T) {
	t.Parallel()

	m := pendingMessage(9, 1)
	m.Subject = "ログインできません"
	m.Body = "画面が戻ってきます。"
	verified := mustVerified(t, "account@example.net")
	m.ReplyTo = &verified

	repo := &fakeRepo{pending: []model.Message{m}}
	sender := &fakeSender{}

	if _, err := NewDispatcher(repo, sender).Dispatch(t.Context()); err != nil {
		t.Fatalf("Dispatch がエラーになった: %v", err)
	}

	if len(sender.autoReplies) != 1 {
		t.Fatalf("控えの件数 = %d, want 1", len(sender.autoReplies))
	}
	r := sender.autoReplies[0]
	if r.ContactID != 9 {
		t.Errorf("ContactID = %d, want 9", r.ContactID)
	}
	if r.To.String() != "account@example.net" {
		t.Errorf("宛先 = %q, want account@example.net", r.To)
	}
	// 控えとして中身が入っていること。
	for _, want := range []string{"問い合わせ番号: 9", "件名: ログインできません", "画面が戻ってきます。"} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("控えに %q が無い:\n%s", want, r.Body)
		}
	}
	// **入力されたアドレスを控えに書き写さないこと。**
	// 入力欄には他人のアドレスが書かれていることがあり、
	// それを本人以外に見せる経路を作らないためです。
	if strings.Contains(r.Body, m.Email) {
		t.Errorf("控えに入力されたアドレスが載っている:\n%s", r.Body)
	}
}

// **控えの送信に失敗しても、問い合わせを送り直さないこと** (best-effort)。
//
// この時点で運営への通知は確定済みです。控えのために行を pending へ
// 戻すと、**同じ問い合わせが上限まで運営に届きます** ——
// 決定 1 が守ると決めたのは運営への通知だけになります。
func TestDispatch_AutoReplyFailureDoesNotRetryTheContact(t *testing.T) {
	t.Parallel()

	m := pendingMessage(3, 1)
	verified := mustVerified(t, "account@example.net")
	m.ReplyTo = &verified

	repo := &fakeRepo{pending: []model.Message{m}}
	sender := &fakeSender{autoReplyErr: errSMTP}

	got, err := NewDispatcher(repo, sender).Dispatch(t.Context())
	if err != nil {
		t.Fatalf("控えの失敗が Dispatch のエラーになった: %v", err)
	}
	if got.Sent != 1 || got.Retried != 0 || got.Failed != 0 {
		t.Errorf("結果 = %+v, want Sent = 1 のみ", got)
	}
	if len(repo.sentIDs) != 1 || repo.sentIDs[0] != 3 {
		t.Errorf("送信済みとして確定されていない: %v", repo.sentIDs)
	}
	if len(repo.rescheduled) != 0 || len(repo.failed) != 0 {
		t.Errorf("控えの失敗で行が差し戻された: rescheduled = %v, failed = %v",
			repo.rescheduled, repo.failed)
	}
}

// **運営への通知が失敗したら、控えも送らないこと。**
//
// 送ってしまうと「受け付けました」と本人に伝えたのに運営には
// 届いていない状態になり、しかも次の周回でもう 1 通控えが届きます。
func TestDispatch_NoAutoReplyWhenNotificationFails(t *testing.T) {
	t.Parallel()

	m := pendingMessage(5, 1)
	verified := mustVerified(t, "account@example.net")
	m.ReplyTo = &verified

	sender := &fakeSender{err: errSMTP}
	repo := &fakeRepo{pending: []model.Message{m}}

	if _, err := NewDispatcher(repo, sender).Dispatch(t.Context()); err != nil {
		t.Fatalf("Dispatch がエラーになった: %v", err)
	}
	if len(sender.autoReplies) != 0 {
		t.Errorf("運営に届いていないのに控えを送った: %+v", sender.autoReplies)
	}
}

// **リースを使い切る前に周回を打ち切ること** (レビュー指摘)。
//
// リースは確保の時点でバッチ全件に一括で掛かります。周回の所要が
// リースを超えると、**まだ処理していない行が他のレプリカから見え**、
// 送信中の行を二重に送られます。
//
// **余白の大きさまで見分けられる粒度にします。** 1 件でリースの半分を使う
// 設定だと、予算が 3/4 でも 1.0 (= 余白なし) でも「2 件で止まる」になり、
// **余白を丸ごと削っても検査が緑のまま**でした (レビュー指摘)。
// 1 接続あたり 1/10 にすると、
//
//	予算 3/4  -> 4 件で 0.8 リース。4 件目のあとで打ち切る
//	予算 1.0  -> 5 件目まで回る (余白なし = 二重送信の窓)
//
// と分かれます。
//
// **控えを持つ行で検査します** (レビュー指摘)。ReplyTo を設定しない行だと
// 送信は 1 接続ぶんで終わり、**leaseBudget が根拠にしている「1 件 = 最悪
// 2 接続」をこの検査が一度も通らない**ことになります。余白の計算を後から
// 詰めても落ちない = 二重送信の窓が開く側で気づけません。
func TestDispatch_StopsBeforeTheLeaseExpires(t *testing.T) {
	t.Parallel()

	const lease = 200 * time.Millisecond

	verified := mustVerified(t, "account@example.net")
	withReply := func(id int64) model.Message {
		m := pendingMessage(id, 1)
		m.ReplyTo = &verified
		return m
	}
	pending := make([]model.Message, 0, 8)
	for id := int64(1); id <= 8; id++ {
		pending = append(pending, withReply(id))
	}
	repo := &fakeRepo{pending: pending}
	// 1 接続あたり。控えがあるので 1 件で 2 回待ちます (= リースの 1/5)。
	sender := &fakeSender{delay: lease / 10}

	d := NewDispatcher(repo, sender)
	d.lease = lease

	got, err := d.Dispatch(t.Context())
	if err != nil {
		t.Fatalf("Dispatch がエラーになった: %v", err)
	}

	// **件数まで見ます。** 「全件ではない」だけだと、控えの往復を数え落として
	// いても、余白を削っていても通ってしまう。
	if got.Total() == 0 {
		t.Fatal("1 件も処理していない (予算が最初から尽きている)")
	}
	if got.Total() > 4 {
		t.Errorf("%d 件処理した。1 件は 2 接続ぶん (リースの 1/5) かかるので、"+
			"予算 3/4 の内側に入るのは 4 件まで —— "+
			"余白が削れているか、控えの往復を費用に数えていない", got.Total())
	}
	// **残りは失われません。** 次の周回で拾い直されるので、
	// 差し戻しも打ち切りも起きていないこと。
	if len(repo.rescheduled) != 0 || len(repo.failed) != 0 {
		t.Errorf("打ち切った行が失敗として扱われた: rescheduled = %v, failed = %v",
			repo.rescheduled, repo.failed)
	}
}

// **空の宛先で控えを送ろうとしないこと** (レビュー指摘)。
//
// VerifiedEmail はゼロ値を作れてしまうので、型だけでは防げません。
func TestDispatch_SkipsAutoReplyWithZeroAddress(t *testing.T) {
	t.Parallel()

	m := pendingMessage(1, 1)
	// **書き忘れを模します。** model.VerifiedEmail{} は
	// フィールドが非公開でもパッケージ外から書けます。
	var zero model.VerifiedEmail
	m.ReplyTo = &zero

	sender := &fakeSender{}
	if _, err := NewDispatcher(&fakeRepo{pending: []model.Message{m}}, sender).
		Dispatch(t.Context()); err != nil {
		t.Fatalf("Dispatch がエラーになった: %v", err)
	}

	if len(sender.autoReplies) != 0 {
		t.Errorf("空の宛先へ控えを送ろうとした: %+v", sender.autoReplies)
	}
}

// mustVerified はテスト用に検証済みアドレスを作ります。
func mustVerified(t *testing.T, raw string) model.VerifiedEmail {
	t.Helper()

	v, err := model.NewVerifiedEmail(raw)
	if err != nil {
		t.Fatalf("NewVerifiedEmail(%q) が失敗した: %v", raw, err)
	}
	return v
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
