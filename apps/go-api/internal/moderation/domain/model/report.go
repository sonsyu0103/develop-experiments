package model

import (
	"fmt"
	"time"

	"develop-experiments/apps/go-api/internal/apperr"
)

// ReportTargetType は通報の対象種別です。
//
// **画像は含めません** (ActionType の TargetType とはここが違います)。
// 画像はコメント / スレッドに従属するので通報の単位にしません ——
// 画像だけを消す判断はモデレーター側が行います。
//
// DB 側の CHECK 制約 (reports_target_type_valid) と同じ 2 値です。
type ReportTargetType string

const (
	// ReportTargetThread はスレッドへの通報です。
	ReportTargetThread ReportTargetType = "thread"
	// ReportTargetComment はコメントへの通報です。
	ReportTargetComment ReportTargetType = "comment"
)

// AllReportTargetTypes は定義済みの対象種別です。
var AllReportTargetTypes = []ReportTargetType{ReportTargetThread, ReportTargetComment}

// ParseReportTargetType は外部から来た文字列を解釈します。
func ParseReportTargetType(s string) (ReportTargetType, error) {
	for _, t := range AllReportTargetTypes {
		if ReportTargetType(s) == t {
			return t, nil
		}
	}
	return "", fmt.Errorf("不正な通報対象です: %w", apperr.ErrInvalidArgument)
}

// ReportReason は通報の理由です。
//
// **自由記述ではなく固定の 4 値**にしています。集計と絞り込みが
// できる形にしておくためで、補足は Note に書きます。
//
// DB 側の CHECK 制約 (reports_reason_valid) と同じ 4 値です。
type ReportReason string

const (
	// ReasonSpam は宣伝・連投です。
	ReasonSpam ReportReason = "spam"
	// ReasonAbuse は誹謗中傷・嫌がらせです。
	ReasonAbuse ReportReason = "abuse"
	// ReasonIllegal は違法な内容です。
	ReasonIllegal ReportReason = "illegal"
	// ReasonOther はその他です。Note に事情を書いてもらいます。
	ReasonOther ReportReason = "other"
)

// AllReportReasons は定義済みの理由です。
var AllReportReasons = []ReportReason{ReasonSpam, ReasonAbuse, ReasonIllegal, ReasonOther}

// ParseReportReason は外部から来た文字列を解釈します。
func ParseReportReason(s string) (ReportReason, error) {
	for _, r := range AllReportReasons {
		if ReportReason(s) == r {
			return r, nil
		}
	}
	return "", fmt.Errorf("不正な通報理由です: %w", apperr.ErrInvalidArgument)
}

// ReportStatus は通報の状態です。
//
// DB 側の CHECK 制約 (reports_status_valid) と同じ 3 値です。
type ReportStatus string

const (
	// ReportOpen は未処理です。この状態のものだけがキューに載ります。
	ReportOpen ReportStatus = "open"
	// ReportResolved は対処済みです。
	ReportResolved ReportStatus = "resolved"
	// ReportRejected は対処不要と判断したものです。
	ReportRejected ReportStatus = "rejected"
)

// AllReportStatuses は定義済みの状態です。
var AllReportStatuses = []ReportStatus{ReportOpen, ReportResolved, ReportRejected}

// ParseReportStatus は外部から来た文字列を解釈します。
func ParseReportStatus(s string) (ReportStatus, error) {
	for _, st := range AllReportStatuses {
		if ReportStatus(s) == st {
			return st, nil
		}
	}
	return "", fmt.Errorf("不正な通報状態です: %w", apperr.ErrInvalidArgument)
}

// IsResolution は「処理済みにする」状態かを返します。
//
// **open は処理ではありません。** 未処理へ戻す経路は作っていないので、
// PATCH の入力としては受け付けません。
func (s ReportStatus) IsResolution() bool {
	switch s {
	case ReportResolved, ReportRejected:
		return true
	case ReportOpen:
		return false
	default:
		return false
	}
}

// Report は 1 件の通報です。
type Report struct {
	ID int64
	// ReporterID は通報した人の内部 ID です。**API には出しません** ——
	// 誰が通報したかをモデレーター以外に見せる必要がなく、
	// 見せると報復の材料になります。
	ReporterID int64

	Target   ReportTargetType
	TargetID int64
	// TargetThreadID は **Target が comment のときだけ**入ります (000009)。
	//
	// comments の主キーが (thread_id, id) なので、これが無いと
	// 通報を受けた側が対象を引けません (8 パーティション全走査)。
	TargetThreadID *int64

	Reason ReportReason
	// Note は補足です。任意なので nil になりえます。
	Note *string

	Status     ReportStatus
	CreatedAt  time.Time
	ResolvedAt *time.Time
	// ResolvedBy は処理した人の内部 ID です。ReporterID と同じく API には出しません。
	ResolvedBy *int64
}
