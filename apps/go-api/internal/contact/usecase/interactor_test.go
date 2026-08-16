package usecase

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"develop-experiments/apps/go-api/internal/apperr"
)

// **受け付けたら保存されること。正規化された値が保存されること。**
func TestSubmit_Saves(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	userID := int64(42)
	at, err := NewInteractor(repo).Submit(t.Context(), SubmitCommand{
		UserID:   &userID,
		Name:     "  ホシノ ",
		Email:    "hoshino@example.com",
		Subject:  "ログインできません",
		Body:     "画面が戻ってきます。\r\n",
		ClientIP: netip.MustParseAddr("192.0.2.1"),
	})
	if err != nil {
		t.Fatalf("受け付けられなかった: %v", err)
	}
	if at.IsZero() {
		t.Error("受理時刻が返っていない")
	}

	if len(repo.saved) != 1 {
		t.Fatalf("保存件数 = %d, want 1", len(repo.saved))
	}
	got := repo.saved[0]
	if got.Name != "ホシノ" {
		t.Errorf("氏名が正規化されていない: %q", got.Name)
	}
	if got.Body != "画面が戻ってきます。" {
		t.Errorf("本文が正規化されていない: %q", got.Body)
	}
	if got.UserID == nil || *got.UserID != userID {
		t.Errorf("投稿者が記録されていない: %v", got.UserID)
	}
}

// **匿名でも受け付けること** (ADR 0008 決定 4)。
//
// 「ログインできない」という問い合わせが来る以上、
// ログインを必須にすると詰みます。
func TestSubmit_AllowsAnonymous(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	if _, err := NewInteractor(repo).Submit(t.Context(), validCommand(nil)); err != nil {
		t.Fatalf("匿名の問い合わせが弾かれた: %v", err)
	}
	if repo.saved[0].UserID != nil {
		t.Error("匿名なのに投稿者が入っている")
	}
}

// **honeypot は破棄されるが、成功と区別がつかないこと** (ADR 0008 決定 4)。
//
// 400 にすると「この項目が引き金だ」とボット側に教えることになり、
// 次の版では空で送られてきます。
func TestSubmit_HoneypotIsDroppedSilently(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	at, err := NewInteractor(repo).Submit(t.Context(), validCommand(func(c *SubmitCommand) {
		c.Honeypot = "http://spam.example.com"
	}))
	if err != nil {
		t.Fatalf("honeypot がエラーになった (成功と区別できてしまう): %v", err)
	}
	if at.IsZero() {
		t.Error("受理時刻が返っていない (成功と応答が変わる)")
	}
	if len(repo.saved) != 0 {
		t.Errorf("破棄されていない: %d 件保存された", len(repo.saved))
	}
}

// **honeypot は入力の検証より先に効くこと。**
//
// あとに置くと、でたらめな入力を埋めたボットには 400 が返り、
// そこから「入力が通れば成功する」ことが分かってしまいます。
func TestSubmit_HoneypotBeatsValidation(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	_, err := NewInteractor(repo).Submit(t.Context(), SubmitCommand{
		// 本文も件名も空 = 本来なら 400 になる入力。
		Honeypot: "x",
	})
	if err != nil {
		t.Errorf("honeypot より検証が先に効いている: %v", err)
	}
}

// **レート制限は集計クエリを打つので、検証を通った入力にだけ払うこと。**
//
// 逆順にすると、でたらめな入力を投げるだけで集計クエリを叩かせられます ——
// レート制限そのものが負荷になります。
func TestSubmit_ValidationBeatsRateLimit(t *testing.T) {
	t.Parallel()

	// countErr が返る = CountRecent が呼ばれたら分かる。
	repo := &fakeRepo{countErr: errors.New("数えてはいけない")}
	_, err := NewInteractor(repo).Submit(t.Context(), validCommand(func(c *SubmitCommand) {
		c.Body = ""
	}))
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Errorf("検証より先にレート制限が走った: %v", err)
	}
}

// **上限に達したら 429 用のエラーになること** (ADR 0013 決定 3)。
func TestSubmit_RateLimited(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{recentCount: 5}
	i := NewInteractor(repo, WithRateLimit(time.Hour, 5))

	_, err := i.Submit(t.Context(), validCommand(nil))
	if !errors.Is(err, apperr.ErrResourceExhausted) {
		t.Fatalf("レート制限に当たらなかった: %v", err)
	}
	if len(repo.saved) != 0 {
		t.Error("弾いたのに保存されている")
	}

	// 上限の 1 つ手前は通る。**境界を「以上」で切っていること**の確認。
	repo.recentCount = 4
	if _, err := i.Submit(t.Context(), validCommand(nil)); err != nil {
		t.Errorf("上限未満が弾かれた: %v", err)
	}
}

// **送信元が分からない場合は素通しすること。**
//
// 経路の都合で IP が取れないときに問い合わせ自体を拒否すると、
// 正規の利用者が詰みます。
func TestSubmit_UnknownIPSkipsRateLimit(t *testing.T) {
	t.Parallel()

	// CountRecent が呼ばれたら失敗する状態にしておく。
	repo := &fakeRepo{countErr: errors.New("IP が無いのに数えた")}
	_, err := NewInteractor(repo).Submit(t.Context(), validCommand(func(c *SubmitCommand) {
		c.ClientIP = netip.Addr{}
	}))
	if err != nil {
		t.Fatalf("送信元不明の問い合わせが弾かれた: %v", err)
	}
	if len(repo.saved) != 1 {
		t.Error("保存されていない")
	}
}

// **0 以下の設定は既定値へ丸めること。**
//
// 丸めないと、窓 0 秒 = 実質無制限 / 上限 0 件 = 誰も送れない、
// のどちらかが静かに成立します。
func TestWithRateLimit_ClampsZero(t *testing.T) {
	t.Parallel()

	i := NewInteractor(&fakeRepo{}, WithRateLimit(0, 0))
	if i.rateWindow != DefaultRateLimitWindow || i.rateMax != DefaultRateLimitMax {
		t.Errorf("既定へ丸められていない: window = %v, max = %d", i.rateWindow, i.rateMax)
	}
}

// validCommand は正しい入力を作り、必要な項目だけを差し替えます。
func validCommand(mutate func(*SubmitCommand)) SubmitCommand {
	c := SubmitCommand{
		Name:     "ホシノ",
		Email:    "hoshino@example.com",
		Subject:  "ログインできません",
		Body:     "画面が戻ってきます。",
		ClientIP: netip.MustParseAddr("192.0.2.1"),
	}
	if mutate != nil {
		mutate(&c)
	}
	return c
}
