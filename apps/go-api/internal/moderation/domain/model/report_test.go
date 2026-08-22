package model

import (
	"errors"
	"regexp"
	"testing"

	"develop-experiments/apps/go-api/internal/apperr"
)

// **知らない値を既定値へ丸めない** (ActionType / Role と同じ約束)。
func TestParseReportEnums(t *testing.T) {
	t.Parallel()

	t.Run("target", func(t *testing.T) {
		t.Parallel()

		for in, want := range map[string]ReportTargetType{
			"thread": ReportTargetThread, "comment": ReportTargetComment,
		} {
			got, err := ParseReportTargetType(in)
			if err != nil || got != want {
				t.Errorf("ParseReportTargetType(%q) = %q, %v", in, got, err)
			}
		}
		// **image は通報の対象ではない。** 画像はコメント / スレッドに
		// 従属するので、通報の単位にしない。
		for _, in := range []string{"", "image", "Thread", "user"} {
			if _, err := ParseReportTargetType(in); !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("ParseReportTargetType(%q) が通ってしまった", in)
			}
		}
	})

	t.Run("reason", func(t *testing.T) {
		t.Parallel()

		for _, in := range []string{"spam", "abuse", "illegal", "other"} {
			if _, err := ParseReportReason(in); err != nil {
				t.Errorf("ParseReportReason(%q) が失敗した: %v", in, err)
			}
		}
		for _, in := range []string{"", "harassment", "SPAM", " spam"} {
			if _, err := ParseReportReason(in); !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("ParseReportReason(%q) が通ってしまった", in)
			}
		}
	})

	t.Run("status", func(t *testing.T) {
		t.Parallel()

		for _, in := range []string{"open", "resolved", "rejected"} {
			if _, err := ParseReportStatus(in); err != nil {
				t.Errorf("ParseReportStatus(%q) が失敗した: %v", in, err)
			}
		}
		for _, in := range []string{"", "closed", "Open"} {
			if _, err := ParseReportStatus(in); !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("ParseReportStatus(%q) が通ってしまった", in)
			}
		}
	})
}

// **未処理へ戻す経路は作っていないこと。**
//
// PATCH の入力として open を受け付けると、
// 「解決した通報を open に戻す」操作ができてしまいます。
// 判断をやり直したい場合は、投稿への操作 (moderation_actions) 側に記録が残ります。
func TestReportStatus_IsResolution(t *testing.T) {
	t.Parallel()

	want := map[ReportStatus]bool{
		ReportResolved: true,
		ReportRejected: true,
		ReportOpen:     false,
	}
	for s, expected := range want {
		if got := s.IsResolution(); got != expected {
			t.Errorf("%q.IsResolution() = %v, want %v", s, got, expected)
		}
	}
	if ReportStatus("closed").IsResolution() {
		t.Error("知らない状態が処理として通っている")
	}
}

// **3 か所の定義が揃っていること** (ActionType と同じ理由)。
//
//	この定数群 / DB の CHECK 制約 (000008) / 仕様書の enum
//
// 揃っていないと「保存できない値をアプリが作る」か
// 「アプリが知らない値が DB に入る」形で本番に出ます。
func TestReportEnums_AgreeAcrossSources(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		declared   map[string]bool
		constraint string
		schema     string
	}{
		{
			name:       "target_type",
			declared:   setOf(AllReportTargetTypes),
			constraint: `CHECK \(target_type IN \(([^)]+)\)\)`,
			schema:     "ReportTargetType",
		},
		{
			name:       "reason",
			declared:   setOf(AllReportReasons),
			constraint: `CHECK \(reason IN \(([^)]+)\)\)`,
			schema:     "ReportReason",
		},
		{
			name:       "status",
			declared:   setOf(AllReportStatuses),
			constraint: `CHECK \(status IN \(([^)]+)\)\)`,
			schema:     "ReportStatus",
		},
	}

	src := readRepoFile(t, "apps/go-api/db/migrations/000008_add_moderation.up.sql")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := regexp.MustCompile(tc.constraint).FindStringSubmatch(src)
			if m == nil {
				t.Fatalf("000008 から %s の CHECK 制約を読み取れなかった", tc.name)
			}
			assertSameSet(t, "DB の CHECK 制約 ("+tc.name+")",
				tc.declared, matchSet(m[1], `'([^']+)'`))
			assertSameSet(t, "仕様書の "+tc.schema,
				tc.declared, valuesFromOpenAPIEnum(t, tc.schema))
		})
	}
}

// setOf は型付きの定数の並びを文字列の集合にします。
func setOf[T ~string](values []T) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[string(v)] = true
	}
	return out
}

// **条件つき制約が、こちらの知っている値だけを使っていること。**
//
//	reports_resolution_complete       (status = 'open' / <> 'open')
//	reports_thread_id_matches_target  (target_type = 'comment')
//
// 列挙の CHECK 制約と違い、**値が本文に直書きされています。**
// `ReportStatusOpen` を改名すると `reports_status_valid` 側は上の
// TestReportEnums_AgreeAcrossSources が落ちて気づけますが、
// **こちらは古い値を参照したまま残ります。**
//
// そのとき壊れるのは「未対処の通報だけが保存できない」という形になり、
// 通報が来るまで表に出ません (ADR 0003 #8)。
func TestConditionalConstraints_UseKnownValues(t *testing.T) {
	t.Parallel()

	// **制約ごとにマイグレーションが違う。** target_thread_id は
	// 000009 で足したもので (ADR 0016 問題 2 の反転)、000008 には無い。
	for _, tc := range []struct {
		constraint string
		migration  string
		known      map[string]bool
	}{
		{
			"reports_resolution_complete",
			"apps/go-api/db/migrations/000008_add_moderation.up.sql",
			setOf(AllReportStatuses),
		},
		{
			"reports_thread_id_matches_target",
			"apps/go-api/db/migrations/000009_fix_reports_queue.up.sql",
			setOf(AllReportTargetTypes),
		},
	} {
		t.Run(tc.constraint, func(t *testing.T) {
			t.Parallel()

			src := readRepoFile(t, tc.migration)
			for _, v := range literalsIn(t, src, tc.constraint) {
				if !tc.known[v] {
					t.Errorf("%s が知らない値 %q を使っている", tc.constraint, v)
				}
			}
		})
	}
}

// literalsIn は名前つき CHECK 制約の本文から、引用符つきの値を全部拾います。
//
// **括弧の対応を数えます。** 条件つき制約は入れ子になっているため、
// 最初の閉じ括弧までを取ると途中で切れます。
func literalsIn(t *testing.T, sql, constraint string) []string {
	t.Helper()

	head := regexp.MustCompile(`CONSTRAINT\s+` + constraint + `\s+CHECK\s*\(`).
		FindStringIndex(sql)
	if head == nil {
		t.Fatalf("%s を読み取れなかった", constraint)
	}
	start := head[1] - 1
	depth, end := 0, -1
	for i := start; i < len(sql) && end < 0; i++ {
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
	}
	if end < 0 {
		t.Fatalf("%s の括弧が閉じていない", constraint)
	}

	var out []string
	for _, m := range regexp.MustCompile(`'([^']*)'`).
		FindAllStringSubmatch(sql[start:end], -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("%s からリテラルを 1 つも拾えなかった", constraint)
	}
	return out
}
