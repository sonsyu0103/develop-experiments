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
