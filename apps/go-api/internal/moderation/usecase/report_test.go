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

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/moderation/domain/model"
	"develop-experiments/apps/go-api/internal/pagination"
)

// fakeReports は通報の受付だけを再現します。
//
// **生存の再現はここではしません。** キューの並びとカーソルは
// SQL が決めるので、フェイクで真似ても実装を検査したことになりません
// (実 DB の検査が持ちます)。ここで見たいのは入力の判断だけです。
type fakeReports struct {
	created  *model.Report
	aliveTh  map[int64]bool
	aliveCmt map[[2]int64]bool
	listErr  error
	// rows は List が返せる行数です (要求された件数で頭打ちになります)。
	rows int
	// gotSize は List に渡された Size です。
	// **1 件多く取っているか**を外から確かめるために持ちます。
	gotSize int32
}

func newFakeReports() *fakeReports {
	return &fakeReports{aliveTh: map[int64]bool{}, aliveCmt: map[[2]int64]bool{}}
}

func (f *fakeReports) Create(_ context.Context, r *model.Report) (*model.Report, bool, error) {
	saved := *r
	saved.ID = 1
	saved.Status = model.ReportOpen
	f.created = &saved
	return &saved, true, nil
}

func (f *fakeReports) List(
	_ context.Context, _ model.ReportStatus, page pagination.Page,
) ([]model.Report, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// **並び順は真似しません** (上のコメントのとおり SQL の担当)。
	// ここで再現するのは「要求された件数だけ返す」ことだけ ——
	// 1 件多く取って「次がある」を判定する側の算術を検査するためです。
	f.gotSize = page.Size
	n := min(int(page.Size), f.rows)
	out := make([]model.Report, 0, n)
	for i := range n {
		out = append(out, model.Report{ID: int64(i + 1), Status: model.ReportOpen})
	}
	return out, nil
}

func (f *fakeReports) Resolve(
	_ context.Context, id int64, status model.ReportStatus, actorID int64,
) (*model.Report, error) {
	return &model.Report{ID: id, Status: status, ResolvedBy: &actorID}, nil
}

func (f *fakeReports) ThreadExists(_ context.Context, id int64) (bool, error) {
	return f.aliveTh[id], nil
}

func (f *fakeReports) CommentExists(_ context.Context, threadID, id int64) (bool, error) {
	return f.aliveCmt[[2]int64{threadID, id}], nil
}

func threadReport() ReportCommand {
	return ReportCommand{
		ReporterID: 7,
		Target:     model.ReportTargetThread,
		TargetID:   1,
		Reason:     model.ReasonSpam,
	}
}

// **匿名では通報できないこと** (ADR 0011 決定 4)。
//
// 匿名で受け付けると、通報そのものが荒らしの手段になります。
func TestReport_RequiresReporter(t *testing.T) {
	t.Parallel()

	repo := newFakeReports()
	cmd := threadReport()
	cmd.ReporterID = 0

	_, _, err := NewReportInteractor(repo, repo).Report(context.Background(), cmd)
	if !errors.Is(err, apperr.ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
	if repo.created != nil {
		t.Error("匿名なのに通報が積まれている")
	}
}

// **対象が生きていないと積まないこと。**
func TestReport_RequiresLiveTarget(t *testing.T) {
	t.Parallel()

	repo := newFakeReports()
	_, _, err := NewReportInteractor(repo, repo).Report(context.Background(), threadReport())
	if !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if repo.created != nil {
		t.Error("存在しない対象の通報が積まれている")
	}
}

// **スレッドの通報にスレッド ID は指定できないこと。**
//
// 受け取ると targetId と threadId が食い違う組み合わせが表現でき、
// DB の CHECK 制約に当たって 500 になります。手前で 400 にします。
func TestReport_ThreadRejectsThreadID(t *testing.T) {
	t.Parallel()

	repo := newFakeReports()
	repo.aliveTh[1] = true
	cmd := threadReport()
	th := int64(1)
	cmd.ThreadID = &th

	_, _, err := NewReportInteractor(repo, repo).Report(context.Background(), cmd)
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}

// **コメントの通報にはスレッド ID が要ること。記録にも残ること。**
func TestReport_CommentCarriesThreadID(t *testing.T) {
	t.Parallel()

	repo := newFakeReports()
	repo.aliveCmt[[2]int64{5, 10}] = true

	cmd := ReportCommand{
		ReporterID: 7, Target: model.ReportTargetComment,
		TargetID: 10, Reason: model.ReasonAbuse,
	}
	interactor := NewReportInteractor(repo, repo)

	if _, _, err := interactor.Report(context.Background(), cmd); !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("スレッド ID 無しの err = %v, want ErrInvalidArgument", err)
	}

	th := int64(5)
	cmd.ThreadID = &th
	got, created, err := interactor.Report(context.Background(), cmd)
	if err != nil || !created {
		t.Fatalf("通報できなかった: err=%v created=%v", err, created)
	}
	// **キューから対象を引くのに要ります。**
	if got.TargetThreadID == nil || *got.TargetThreadID != 5 {
		t.Errorf("target_thread_id = %v, want 5", got.TargetThreadID)
	}
}

// **補足の正規化。** 空白だけは nil に丸めます。
func TestReport_NormalizesNote(t *testing.T) {
	t.Parallel()

	ptr := func(s string) *string { return &s }
	repo := newFakeReports()
	repo.aliveTh[1] = true
	interactor := NewReportInteractor(repo, repo)

	cmd := threadReport()
	cmd.Note = ptr("   \n ")
	if _, _, err := interactor.Report(context.Background(), cmd); err != nil {
		t.Fatalf("通報できなかった: %v", err)
	}
	if repo.created.Note != nil {
		t.Errorf("note = %q, want nil", *repo.created.Note)
	}

	// **文字数で数える。** バイト数だと日本語が 1/3 の長さで弾かれます。
	ok := strings.Repeat("あ", maxNoteLength)
	cmd.Note = &ok
	if _, _, err := interactor.Report(context.Background(), cmd); err != nil {
		t.Fatalf("上限ちょうどの日本語が弾かれた: %v", err)
	}

	over := strings.Repeat("あ", maxNoteLength+1)
	cmd.Note = &over
	if _, _, err := interactor.Report(context.Background(), cmd); !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}

// **キューと処理は moderator 以上だけであること。**
func TestReportQueue_RequiresModerator(t *testing.T) {
	t.Parallel()

	repo := newFakeReports()
	interactor := NewReportInteractor(repo, repo)
	plain := model.Actor{UserID: 7}

	if _, err := interactor.ListQueue(
		context.Background(), plain, model.ReportOpen, pagination.Page{Size: 20},
	); !errors.Is(err, apperr.ErrPermissionDenied) {
		t.Errorf("ListQueue の err = %v, want ErrPermissionDenied", err)
	}
	if _, err := interactor.Resolve(
		context.Background(), plain, 1, model.ReportResolved,
	); !errors.Is(err, apperr.ErrPermissionDenied) {
		t.Errorf("Resolve の err = %v, want ErrPermissionDenied", err)
	}
}

// **未処理へ戻す指定は受け付けないこと。**
func TestReportResolve_RejectsOpen(t *testing.T) {
	t.Parallel()

	repo := newFakeReports()
	_, err := NewReportInteractor(repo, repo).
		Resolve(context.Background(), moderator(), 1, model.ReportOpen)
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}

// **補足の上限が 3 か所で揃っていること** (理由の上限と同じ理由)。
func TestMaxNoteLength_AgreesAcrossSources(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "..", "..", "..")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s を読めない: %v", rel, err)
		}
		return string(b)
	}

	migration := read("apps/go-api/db/migrations/000008_add_moderation.up.sql")
	m := regexp.MustCompile(`char_length\(note\) <= (\d+)`).FindStringSubmatch(migration)
	if m == nil {
		t.Fatal("000008 から note の上限を読み取れなかった")
	}
	if got, _ := strconv.Atoi(m[1]); got != maxNoteLength {
		t.Errorf("DB の CHECK 制約 = %d, Go = %d", got, maxNoteLength)
	}

	spec := read("api/openapi.yaml")
	block := regexp.MustCompile(`(?ms)^        note:\n          type: string\n          maxLength: (\d+)`).
		FindStringSubmatch(spec)
	if block == nil {
		t.Fatal("openapi.yaml から note の maxLength を読み取れなかった")
	}
	if got, _ := strconv.Atoi(block[1]); got != maxNoteLength {
		t.Errorf("仕様書の maxLength = %d, Go = %d", got, maxNoteLength)
	}
}

// **切り詰めと次カーソルの算術** (レビュー指摘: ここに検査が 1 本も無かった)。
//
// フェイクの List が常に nil を返していたため、
// 「1 件多く取る」も「余ったら切る」も一度も動いていませんでした。
// 並び順は SQL の担当なので実 DB の検査に任せますが、
// **件数の勘定はユースケース側の算術**なので、ここで固定します。
func TestListQueue_TruncatesAndSetsCursor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		size     int32
		rows     int
		wantLen  int
		wantNext bool
	}{
		{name: "ちょうど埋まる (次は無い)", size: 3, rows: 3, wantLen: 3, wantNext: false},
		{name: "余りがある (次がある)", size: 3, rows: 10, wantLen: 3, wantNext: true},
		{name: "足りない", size: 3, rows: 1, wantLen: 1, wantNext: false},
		{name: "1 件も無い", size: 3, rows: 0, wantLen: 0, wantNext: false},
		// **Size が 0 でも既定値へ丸めること。**
		//
		// 丸めをやめても panic はしません (レビュー指摘) ——
		// limit が 0 になっても `limit > 0 &&` が先に短絡するためで、
		// 空スライスへの添字には到達しない。**丸めが守っているのは
		// 「1 件も返らない」ほう**で、キューが常に空に見える形になります。
		{name: "Size が 0 なら既定値へ丸める", size: 0, rows: 200, wantLen: int(pagination.DefaultSize), wantNext: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reports := newFakeReports()
			reports.rows = tt.rows
			interactor := NewReportInteractor(reports, reports)

			got, err := interactor.ListQueue(t.Context(),
				model.Actor{UserID: 1, CanModerate: true},
				model.ReportOpen,
				pagination.Page{Size: tt.size},
			)
			if err != nil {
				t.Fatalf("ListQueue がエラーになった: %v", err)
			}

			if len(got.Reports) != tt.wantLen {
				t.Errorf("件数 = %d, want %d", len(got.Reports), tt.wantLen)
			}
			if (got.NextCursor != nil) != tt.wantNext {
				t.Errorf("NextCursor = %v, want ある: %v", got.NextCursor, tt.wantNext)
			}
			// **1 件多く取っていること。** ここが 1 のままだと
			// 「次がある」の判定が常に偽になり、2 ページ目が出ない。
			want := tt.size
			if want < 1 {
				want = pagination.DefaultSize
			}
			if reports.gotSize != want+1 {
				t.Errorf("List に渡した Size = %d, want %d (1 件多く取っていない)",
					reports.gotSize, want+1)
			}
		})
	}
}
