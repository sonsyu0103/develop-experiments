package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/pagination"
	"develop-experiments/apps/go-api/internal/thread/domain/model"
)

// ---------------------------------------------------------------------------
// 実 DB に対する検証: スレッド検索 (Phase 11 / ADR 0012)
// ---------------------------------------------------------------------------
//
// **フェイクでは測れないものが 3 つあります。**
//
//   - LIKE のエスケープ。フェイクは strings.Contains で真似ているので、
//     `%` が全件一致になるかどうかは**実際に SQL を投げないと分かりません**
//   - ILIKE の大小同一視。ここも実装が PostgreSQL 側にあります
//   - 部分索引の述語。削除済みのスレッドを検索結果から外しているのは
//     WHERE deleted_at IS NULL で、SQL を通らないと検証になりません
//
// **索引が使われるかどうかは、ここでは見ません。** 計画は行数と統計で
// 変わるため、開発データの規模で EXPLAIN を検査に使うと
// 「本番と違う計画に対する検査」になります (実測は ADR 0012 に残しました)。

// seedThreadTitled は指定したタイトルのスレッドを 1 件作ります。
func seedThreadTitled(t *testing.T, pool *pgxpool.Pool, title string) int64 {
	t.Helper()

	var id int64
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO threads (title) VALUES ($1) RETURNING id`, title).Scan(&id); err != nil {
		t.Fatalf("スレッドを作れませんでした: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM comments WHERE thread_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM threads WHERE id = $1`, id)
	})
	return id
}

// searchIDs は検索を実行して id の並びを返します。
func searchIDs(t *testing.T, repo *ThreadRepository, keyword string) []int64 {
	t.Helper()

	q, err := model.ParseSearchQuery(&keyword)
	if err != nil {
		t.Fatalf("検索語を組み立てられませんでした (%q): %v", keyword, err)
	}
	if q == nil {
		t.Fatalf("検索語が空になりました (%q)", keyword)
	}

	page, err := pagination.NewPage(nil, 100)
	if err != nil {
		t.Fatalf("ページ指定を組み立てられませんでした: %v", err)
	}

	got, err := repo.SearchSummaries(t.Context(), *q, page)
	if err != nil {
		t.Fatalf("SearchSummaries が失敗した: %v", err)
	}
	ids := make([]int64, 0, len(got))
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	return ids
}

// contains は id が結果に含まれるかを返します。
//
// **「結果がこの並びと完全に一致する」では検査しません。**
// 開発データやシードが同じ語を含んでいると、無関係な理由で落ちます。
func contains(ids []int64, id int64) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// **ワイルドカードが打ち消されていること。**
//
// ここが抜けると、利用者は `%` の 1 文字で全件を引けます (ADR 0012 の罠)。
// 「1 文字で全表走査」は検索でいちばん高くつく壊れ方になります。
func TestThreadRepository_Search_EscapesWildcards_Live(t *testing.T) {
	pool := liveDB(t)
	repo := NewThreadRepository(pool)

	plain := seedThreadTitled(t, pool, "live 検索 ふつうのタイトル")
	percent := seedThreadTitled(t, pool, "live 検索 100% 達成")
	under := seedThreadTitled(t, pool, "live 検索 a_b の話")
	backslash := seedThreadTitled(t, pool, `live 検索 c\d の話`)

	t.Run("% は全件一致にならない", func(t *testing.T) {
		ids := searchIDs(t, repo, "%")
		if contains(ids, plain) {
			t.Errorf("`%%` がワイルドカードとして効いている (件数 %d)", len(ids))
		}
		// 文字としての % は引ける。
		if !contains(ids, percent) {
			t.Error("文字としての `%` を含むタイトルが引けない")
		}
	})

	t.Run("_ は任意の 1 文字にならない", func(t *testing.T) {
		ids := searchIDs(t, repo, "_")
		if contains(ids, plain) {
			t.Errorf("`_` がワイルドカードとして効いている (件数 %d)", len(ids))
		}
		if !contains(ids, under) {
			t.Error("文字としての `_` を含むタイトルが引けない")
		}
	})

	t.Run("バックスラッシュはエスケープ文字として解釈されない", func(t *testing.T) {
		// `\` を素通しすると ESCAPE '\' に食われ、続く 1 文字の
		// 特別扱いを利用者に奪われます。
		ids := searchIDs(t, repo, `c\d`)
		if !contains(ids, backslash) {
			t.Error(`c\d を含むタイトルが引けない`)
		}
		if contains(ids, plain) {
			t.Error("無関係なタイトルまで一致している")
		}
	})

	t.Run("%_ の並びも文字として扱われる", func(t *testing.T) {
		// エスケープを順に ReplaceAll していると、ここで二重になります。
		if ids := searchIDs(t, repo, "%"); !contains(ids, percent) {
			t.Error("二重エスケープで引けなくなっている")
		}
	})
}

// **大文字小文字を区別しないこと** (ILIKE)。
// LIKE に変えると、この検査だけが落ちます。
func TestThreadRepository_Search_CaseInsensitive_Live(t *testing.T) {
	pool := liveDB(t)
	repo := NewThreadRepository(pool)

	id := seedThreadTitled(t, pool, "live PostgreSQL の検索")

	for _, keyword := range []string{"PostgreSQL", "postgresql", "POSTGRESQL", "pOsTgReSqL"} {
		if !contains(searchIDs(t, repo, keyword), id) {
			t.Errorf("q=%q で引けない", keyword)
		}
	}
}

// **削除済みのスレッドは検索結果に出ないこと。**
//
// 述語は SQL 側 (WHERE deleted_at IS NULL) にあり、
// 部分索引 threads_title_trgm_idx の述語とも揃えてあります。
func TestThreadRepository_Search_SkipsDeleted_Live(t *testing.T) {
	pool := liveDB(t)
	repo := NewThreadRepository(pool)

	alive := seedThreadTitled(t, pool, "live 削除検査 生きている")
	deleted := seedThreadTitled(t, pool, "live 削除検査 消えている")

	if _, err := pool.Exec(t.Context(),
		`UPDATE threads SET deleted_at = now() WHERE id = $1`, deleted); err != nil {
		t.Fatalf("論理削除できませんでした: %v", err)
	}

	ids := searchIDs(t, repo, "live 削除検査")
	if !contains(ids, alive) {
		t.Error("生きているスレッドが検索結果に出ない")
	}
	if contains(ids, deleted) {
		t.Error("論理削除したスレッドが検索結果に出ている")
	}
}

// **絞り込んだうえで新着順・カーソルが効くこと** (ADR 0012 決定 3)。
//
// カーソルは検索結果の最後の id を指します。
// 絞り込む前の id で切ると、2 ページ目に一致しない行が現れます。
func TestThreadRepository_Search_Paginates_Live(t *testing.T) {
	pool := liveDB(t)
	repo := NewThreadRepository(pool)

	// 一致するのは 3 件。間に一致しないスレッドを挟んで、
	// カーソルが「絞り込んだ結果の id」であることを見えるようにします。
	first := seedThreadTitled(t, pool, "live ページ送り検査 1")
	seedThreadTitled(t, pool, "live 無関係なスレッド")
	second := seedThreadTitled(t, pool, "live ページ送り検査 2")
	seedThreadTitled(t, pool, "live これも無関係")
	third := seedThreadTitled(t, pool, "live ページ送り検査 3")

	keyword := "live ページ送り検査"
	q, err := model.ParseSearchQuery(&keyword)
	if err != nil || q == nil {
		t.Fatalf("検索語を組み立てられませんでした: %v", err)
	}

	page, err := pagination.NewPage(nil, 2)
	if err != nil {
		t.Fatalf("ページ指定を組み立てられませんでした: %v", err)
	}
	got, err := repo.SearchSummaries(t.Context(), *q, page)
	if err != nil {
		t.Fatalf("1 ページ目が失敗した: %v", err)
	}
	if len(got) != 2 || got[0].ID != third || got[1].ID != second {
		t.Fatalf("1 ページ目 = %v, want [%d %d]", idsOf(got), third, second)
	}

	token, err := pagination.NewCursor(second).Encode()
	if err != nil {
		t.Fatalf("カーソルを組み立てられませんでした: %v", err)
	}
	page2, err := pagination.NewPage(&token, 2)
	if err != nil {
		t.Fatalf("ページ指定を組み立てられませんでした: %v", err)
	}
	got2, err := repo.SearchSummaries(t.Context(), *q, page2)
	if err != nil {
		t.Fatalf("2 ページ目が失敗した: %v", err)
	}
	if len(got2) != 1 || got2[0].ID != first {
		t.Fatalf("2 ページ目 = %v, want [%d]", idsOf(got2), first)
	}
}

func idsOf(summaries []model.Summary) []int64 {
	ids := make([]int64, 0, len(summaries))
	for _, s := range summaries {
		ids = append(ids, s.ID)
	}
	return ids
}
