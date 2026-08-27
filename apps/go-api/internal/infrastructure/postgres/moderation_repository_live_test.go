package postgres

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/apperr"
	imagemodel "develop-experiments/apps/go-api/internal/image/domain/model"
	moderationmodel "develop-experiments/apps/go-api/internal/moderation/domain/model"
	moderationrepo "develop-experiments/apps/go-api/internal/moderation/domain/repository"
)

// ---------------------------------------------------------------------------
// 実 DB に対する検証: モデレーション (docs/adr/0011-moderation.md)
// ---------------------------------------------------------------------------
//
// **`status = 'deleted'` を書く経路は、ここが初めての実 DB 実行になります。**
//
// Phase 6 で回収バッチは入りましたが、書く側 (モデレーターの削除) が
// 無かったため、`deleted` を通していたのは
//
//   - ユニットテスト  : フェイクのリポジトリ。SQL は通らない
//   - スモーク        : `UPDATE images SET status='deleted'` を SQL で直接書いていた
//   - 実 DB 検査      : 同上
//
// の 3 つだけでした。つまり **「アプリのクエリが status を書き替え、
// その行を回収バッチが拾う」を端から端まで通した実行が 1 度もありませんでした。**
// この節がその穴を埋めます。

// seedComment はコメントを 1 件作り、その ID を返します。
func seedComment(t *testing.T, pool *pgxpool.Pool, threadID int64) int64 {
	t.Helper()

	var id int64
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO comments (thread_id, seq, author_name, body)
		VALUES ($1, 1, '名無しさん', 'live: 削除される投稿')
		RETURNING id`, threadID).Scan(&id); err != nil {
		t.Fatalf("コメントを作れませんでした: %v", err)
	}
	return id
}

// actionsFor は対象に紐づくモデレーション記録の件数を返します。
func actionsFor(t *testing.T, pool *pgxpool.Pool, targetType, targetID string) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM moderation_actions
		WHERE target_type = $1 AND target_id = $2`, targetType, targetID).Scan(&n); err != nil {
		t.Fatalf("記録を数えられませんでした: %v", err)
	}
	return n
}

// cleanupActions は検証で書いた記録を消します。
func cleanupActions(t *testing.T, pool *pgxpool.Pool, actorID int64) {
	t.Helper()

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM moderation_actions WHERE actor_id = $1`, actorID)
	})
}

// **アプリのクエリで消した画像が、そのまま回収対象になること。**
//
// ここが「モデレーターが削除 → 回収バッチが S3 から消す」の継ぎ目です。
// S3 側 (実 MinIO への DELETE) は objectstorage の live テストが、
// 回収の手順は reclaim_test.go が見ています。
// **この 3 つが繋がっていることを、DB 側で確かめるのがこの検査です。**
func TestModerationRepository_MarkImageDeleted_MakesReclaimable_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900400)
	seedOwner(t, pool, ownerID)

	repo := NewModerationRepository(pool)
	images := NewImageRepository(pool)

	// **添付済みの画像を作る。** 未添付なら attached_at IS NULL で
	// 元から回収対象なので、status を書き替えた効果が見えません。
	id := insertImage(t, pool, ownerID, imagemodel.StatusCommitted, 2*time.Hour)
	if _, err := pool.Exec(t.Context(),
		`UPDATE images SET attached_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("添付済みにできませんでした: %v", err)
	}

	if _, ok := reclaimableIDs(t, images)[id]; ok {
		t.Fatal("添付済みの committed が最初から回収対象になっている (前提が壊れている)")
	}

	if err := repo.MarkImageDeleted(t.Context(), id); err != nil {
		t.Fatalf("MarkImageDeleted が失敗した: %v", err)
	}

	got, ok := reclaimableIDs(t, images)[id]
	if !ok {
		t.Fatal("削除した画像が回収対象になっていない")
	}
	if got != imagemodel.StatusDeleted {
		t.Errorf("status = %q, want deleted", got)
	}
}

// **まだアップロード中の画像も削除できること。**
//
// db/query/images.sql の MarkImageDeleted は 'pending' も対象にすると
// 明記し、そのうえで「消すと回収の猶予が外れる」競合まで分析して残すと
// 決めている。ところが 000006 の CHECK 制約が
//
//	(status = 'pending'  AND committed_at IS NULL) OR
//	(status <> 'pending' AND committed_at IS NOT NULL)
//
// だったため、**'pending' を 'deleted' にするとどちらの枝も満たさず
// 23514 で落ちていた** (レビュー指摘)。translateError は 500 を返すので、
// モデレーターには「サーバエラー」としか見えない。
//
// **既存の検査はすべて StatusCommitted で作っていたので、
// この経路は CI で 1 度も通っていなかった。** 000013 で制約を直し、
// ここで経路を押さえる。
func TestModerationRepository_MarkImageDeleted_AcceptsPending_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900403)
	seedOwner(t, pool, ownerID)

	repo := NewModerationRepository(pool)

	id := insertImage(t, pool, ownerID, imagemodel.StatusPending, 2*time.Hour)
	if err := repo.MarkImageDeleted(t.Context(), id); err != nil {
		t.Fatalf("pending の画像を削除できなかった: %v", err)
	}

	var status string
	var committedAt *time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT status, committed_at FROM images WHERE id = $1`, id,
	).Scan(&status, &committedAt); err != nil {
		t.Fatalf("状態を読めなかった: %v", err)
	}
	if status != string(imagemodel.StatusDeleted) {
		t.Errorf("status = %q, want deleted", status)
	}
	// **committed_at は埋めない。** 「アップロードが完了した時刻」なので、
	// 完了していない画像に入れるのは嘘になる (000013 の判断)。
	if committedAt != nil {
		t.Errorf("committed_at = %v, want nil (完了していないので入れてはいけない)", *committedAt)
	}
}

// **削除した画像には猶予を与えないこと** (Phase 10 後半で直した)。
//
// 初版は 3 種類すべてに created_at の足切りをかけていたため、
// 直前にアップロードされた画像をモデレーターが削除しても、
// **作成から 1 時間経つまで S3 に残り続けていました。**
// ADR 0011 決定 5 は「URL を直接叩けば見え続ける」ことを消す理由に
// 挙げているので、そこに 1 時間の穴が空いていたことになります。
//
// 削除は明示的な操作であり、「まだ判断がついていない」状態がありません。
func TestModerationRepository_DeletedImageIgnoresGrace_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900401)
	seedOwner(t, pool, ownerID)

	repo := NewModerationRepository(pool)
	images := NewImageRepository(pool)

	// **たった今アップロードされた画像。** 猶予 (1 時間) の内側にある。
	fresh := insertImage(t, pool, ownerID, imagemodel.StatusCommitted, time.Second)

	// 前提: この時点では猶予に守られて対象外。
	if _, ok := reclaimableIDs(t, images)[fresh]; ok {
		t.Fatal("アップロード直後の committed が回収対象になっている (猶予が効いていない)")
	}

	if err := repo.MarkImageDeleted(t.Context(), fresh); err != nil {
		t.Fatalf("MarkImageDeleted が失敗した: %v", err)
	}

	if _, ok := reclaimableIDs(t, images)[fresh]; !ok {
		t.Error("削除直後の画像が回収対象になっていない (猶予が deleted にも効いている)")
	}
}

// **2 回目の削除は 0 行になること。**
//
// 成功として扱うと、実際には何も起きていない操作が記録に残ります。
// object_reclaimed_at が入った行も同じく対象外です ——
// 実体が既に無いので、記録だけが増えることになります。
func TestModerationRepository_MarkImageDeleted_SecondTimeIsNotFound_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900402)
	seedOwner(t, pool, ownerID)

	repo := NewModerationRepository(pool)

	id := insertImage(t, pool, ownerID, imagemodel.StatusCommitted, 2*time.Hour)
	if err := repo.MarkImageDeleted(t.Context(), id); err != nil {
		t.Fatalf("1 回目が失敗した: %v", err)
	}
	if err := repo.MarkImageDeleted(t.Context(), id); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("2 回目の err = %v, want ErrNotFound", err)
	}

	// 回収済みの行も対象外。
	reclaimed := insertImage(t, pool, ownerID, imagemodel.StatusCommitted, 2*time.Hour)
	if _, err := pool.Exec(t.Context(),
		`UPDATE images SET object_reclaimed_at = now() WHERE id = $1`, reclaimed); err != nil {
		t.Fatalf("回収済みにできませんでした: %v", err)
	}
	if err := repo.MarkImageDeleted(t.Context(), reclaimed); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("回収済みの err = %v, want ErrNotFound", err)
	}
}

// **削除と記録が同じトランザクションに収まること。**
//
// 記録に失敗したら削除も残ってはいけません。分かれていると
// 「誰が消したか分からない投稿」ができ、ADR 0010 の理由で
// ログでは代替できません。
//
// **実 DB でしか確かめられません。** フェイクの WithinTx は
// 巻き戻しを手で真似ているだけで、ROLLBACK そのものは通りません。
func TestModerationRepository_DeleteAndRecordAreAtomic_Live(t *testing.T) {
	pool := liveDB(t)
	const actorID = int64(900403)
	seedOwner(t, pool, actorID)
	cleanupActions(t, pool, actorID)

	// **ID は DB に採らせる。** 固定値にすると、前回の実行が
	// 後片付け前に落ちたときに ON CONFLICT の分岐へ入り、
	// 「消えているのに生きている」前提で走ることになる。
	threadID := seedThread(t, pool, nil)
	targetID := strconv.FormatInt(threadID, 10)

	repo := NewModerationRepository(pool)
	alive := func() bool {
		var deletedAt *time.Time
		if err := pool.QueryRow(t.Context(),
			`SELECT deleted_at FROM threads WHERE id = $1`, threadID).Scan(&deletedAt); err != nil {
			t.Fatalf("スレッドを読めませんでした: %v", err)
		}
		return deletedAt == nil
	}

	// --- 記録が失敗する場合 ---
	//
	// **CHECK 制約に落ちる値を渡して失敗させる。** フェイクのエラーでは
	// 「アプリが返したエラーで巻き戻る」ことしか見られず、
	// DB 側のエラーで巻き戻るかは通りません。
	sentinel := errors.New("記録に失敗しました")
	err := repo.WithinTx(t.Context(), func(tx moderationrepo.Repository) error {
		if delErr := tx.SoftDeleteThread(t.Context(), threadID); delErr != nil {
			return delErr
		}
		_, recErr := tx.RecordAction(t.Context(), &moderationmodel.Action{
			ActorID: actorID,
			// DB の CHECK 制約 (moderation_action_valid) に無い値。
			Type:     moderationmodel.ActionType("ban_user"),
			Target:   moderationmodel.TargetThread,
			TargetID: targetID,
		})
		if recErr != nil {
			return sentinel
		}
		t.Error("CHECK 制約に無い action が保存できてしまった")
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (CHECK 制約で落ちるはず)", err)
	}
	if !alive() {
		t.Error("記録に失敗したのにスレッドが論理削除されたままになっている")
	}

	// --- 成功する場合 ---
	err = repo.WithinTx(t.Context(), func(tx moderationrepo.Repository) error {
		if delErr := tx.SoftDeleteThread(t.Context(), threadID); delErr != nil {
			return delErr
		}
		_, recErr := tx.RecordAction(t.Context(), &moderationmodel.Action{
			ActorID:  actorID,
			Type:     moderationmodel.ActionDeleteThread,
			Target:   moderationmodel.TargetThread,
			TargetID: targetID,
		})
		return recErr
	})
	if err != nil {
		t.Fatalf("削除と記録が失敗した: %v", err)
	}
	if alive() {
		t.Error("スレッドが論理削除されていない")
	}
	if n := actionsFor(t, pool, "thread", targetID); n != 1 {
		t.Errorf("記録が %d 件。1 件であるべき", n)
	}

	// 2 回目は 0 行 = ErrNotFound。
	if err := repo.SoftDeleteThread(t.Context(), threadID); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("2 回目の削除 err = %v, want ErrNotFound", err)
	}
}

// **コメントの削除がパーティションキーで絞れること。**
//
// 主キーが (thread_id, id) なので、スレッド ID が違えば当たりません。
// この検査が落ちるときは、パラメータの順序が入れ替わっているか、
// クエリが thread_id を落としています。
func TestModerationRepository_SoftDeleteComment_Live(t *testing.T) {
	pool := liveDB(t)
	threadID := seedThread(t, pool, nil)

	repo := NewModerationRepository(pool)
	commentID := seedComment(t, pool, threadID)

	// **違うスレッド ID では当たらない。**
	if err := repo.SoftDeleteComment(t.Context(), threadID+1, commentID); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("別スレッドの ID で消せてしまった (err=%v)", err)
	}

	if err := repo.SoftDeleteComment(t.Context(), threadID, commentID); err != nil {
		t.Fatalf("SoftDeleteComment が失敗した: %v", err)
	}
	if err := repo.SoftDeleteComment(t.Context(), threadID, commentID); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("2 回目の err = %v, want ErrNotFound", err)
	}
}

// **記録が読み出せる形で保存されること。**
//
// 理由の NULL と非 NULL の両方を通します。
// reason は *string なので、詰め替えを 1 つ間違えると
// 「理由なし」が空文字で保存される形になります。
func TestModerationRepository_RecordAction_Live(t *testing.T) {
	pool := liveDB(t)
	const actorID = int64(900405)
	seedOwner(t, pool, actorID)
	cleanupActions(t, pool, actorID)

	repo := NewModerationRepository(pool)
	reason := "誹謗中傷のため"
	imageID := uuid.New().String()

	withReason, err := repo.RecordAction(t.Context(), &moderationmodel.Action{
		ActorID:  actorID,
		Type:     moderationmodel.ActionDeleteImage,
		Target:   moderationmodel.TargetImage,
		TargetID: imageID,
		Reason:   &reason,
	})
	if err != nil {
		t.Fatalf("RecordAction が失敗した: %v", err)
	}
	if withReason.ID == 0 {
		t.Error("採番された ID が返っていない")
	}
	if withReason.Reason == nil || *withReason.Reason != reason {
		t.Errorf("reason = %v, want %q", withReason.Reason, reason)
	}
	if withReason.CreatedAt.IsZero() {
		t.Error("created_at が入っていない")
	}

	withoutReason, err := repo.RecordAction(t.Context(), &moderationmodel.Action{
		ActorID:  actorID,
		Type:     moderationmodel.ActionDeleteThread,
		Target:   moderationmodel.TargetThread,
		TargetID: "1",
	})
	if err != nil {
		t.Fatalf("理由なしの RecordAction が失敗した: %v", err)
	}
	// **空文字に化けないこと。** 化けると「理由を書いた」記録と
	// 区別できなくなります。
	if withoutReason.Reason != nil {
		t.Errorf("reason = %q, want nil", *withoutReason.Reason)
	}
}

// **DB 側の CHECK 制約が効いていること。**
//
// アプリと DB のどちらか片方だけ値を増やすと、ここで気づけます
// (users_role_valid をスモークで検査しているのと同じ形)。
func TestModerationRepository_RejectsUnknownValues_Live(t *testing.T) {
	pool := liveDB(t)
	const actorID = int64(900406)
	seedOwner(t, pool, actorID)
	cleanupActions(t, pool, actorID)

	repo := NewModerationRepository(pool)
	tooLong := make([]rune, 501)
	for i := range tooLong {
		tooLong[i] = 'あ'
	}
	long := string(tooLong)

	tests := []struct {
		name   string
		action moderationmodel.Action
	}{
		{"知らない action", moderationmodel.Action{
			ActorID: actorID, Type: "ban_user",
			Target: moderationmodel.TargetUser, TargetID: "1",
		}},
		{"知らない target_type", moderationmodel.Action{
			ActorID: actorID, Type: moderationmodel.ActionDeleteThread,
			Target: "post", TargetID: "1",
		}},
		{"長すぎる理由", moderationmodel.Action{
			ActorID: actorID, Type: moderationmodel.ActionDeleteThread,
			Target: moderationmodel.TargetThread, TargetID: "1", Reason: &long,
		}},
		{"空の target_id", moderationmodel.Action{
			ActorID: actorID, Type: moderationmodel.ActionDeleteThread,
			Target: moderationmodel.TargetThread, TargetID: "",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.action
			if _, err := repo.RecordAction(t.Context(), &a); err == nil {
				t.Error("DB が受け入れてしまった (CHECK 制約が効いていない)")
			}
		})
	}
}
