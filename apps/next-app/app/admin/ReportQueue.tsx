'use client';

// 通報キュー。**古い順**に積まれたものを上から処理します。
//
// 【キューだけカーソルの向きが逆】
// 他の一覧は新しい順 (`id < cursor`) ですが、キューは古い順 (`id > cursor`) です。
// 滞留したときに最初に届いたものから処理されるようにするためで、
// **取り違えると 2 ページ目から常に空**になります
// (ADR 0011 の「実装して分かったこと 6」)。
// 向きは API が決めるので、ここは `nextCursor` をそのまま返すだけです。
//
// 【「処理済みにする」と「削除する」を分けている】
// 仕様書がそう決めています —— まとめると、キューを片付ける操作が
// そのまま削除になり、誤操作が投稿に届きます。
// 画面でも 2 つのボタンに分けて、片方を押しても他方は起きません。
import { useCallback, useEffect, useRef, useState } from 'react';
import Link from 'next/link';

import {
  ApiError,
  createModerationAction,
  getThread,
  listReports,
  resolveReport,
  type Report,
  type ReportReason,
  type ReportStatus,
  type Thread,
} from '../lib/api';
import { describe } from '../lib/errors';
import { formatTime } from '../lib/ui';

const pageSize = 20;

const reasonLabel: Record<ReportReason, string> = {
  spam: 'スパム',
  abuse: '誹謗中傷',
  illegal: '違法',
  other: 'その他',
};

const statusLabel: Record<ReportStatus, string> = {
  open: '未処理',
  resolved: '対処した',
  rejected: '対処不要',
};

/**
 * 通報からスレッド ID を取り出します。
 *
 * **コメントの通報にだけ `threadId` が現れます** (スレッドの通報では
 * キーごと省略されます —— null ではありません)。
 * `comments` は `thread_id` によるパーティションなので、
 * これが無いとコメントは削除も表示もできません。
 */
function threadIdOf(r: Report): number | undefined {
  return r.targetType === 'thread' ? r.targetId : r.threadId;
}

type ThreadState = Thread | 'missing' | 'failed';

export function ReportQueue() {
  const [status, setStatus] = useState<ReportStatus>('open');
  const [reports, setReports] = useState<Report[]>([]);
  const [cursor, setCursor] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // 通報 ID ごとの「直前の操作の結果」。押した操作が効いたことを、
  // 一覧を引き直さずに伝えるために持ちます。
  const [results, setResults] = useState<Record<number, string>>({});
  const [reasons, setReasons] = useState<Record<number, string>>({});

  // **処理中の行は 1 つとは限りません** (レビュー指摘)。
  // 単一の ID で持つと、A の処理中に B を押した時点で A が上書きされ、
  // A が先に終わった `finally` が **B のボタンを押せる状態に戻します。**
  // 削除で二重送信が通る導線になるので、集合で持ちます。
  const [busy, setBusy] = useState<ReadonlySet<number>>(new Set());

  const setRowBusy = (id: number, on: boolean) =>
    setBusy((prev) => {
      const next = new Set(prev);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });

  // 対象のスレッド。**通報は ID しか持っていない**ので、
  // これが無いとモデレーターは数字だけを見て判断することになります。
  const [threads, setThreads] = useState<Record<number, ThreadState>>({});
  // 取得済みのスレッド ID。同じスレッドへの通報が並ぶと
  // (荒らしでは実際に並びます) 同じ GET を何度も投げるため。
  const fetched = useRef(new Set<number>());

  // **取得の世代** (レビュー指摘)。
  //
  // 読み込み中でも絞り込みは操作できるので、
  // 「次のページ」の直後に絞り込みを変えると、**遅れて返った古い応答が
  // 新しい一覧に追記されます** —— 見出しは「対処した」なのに
  // 未処理の行が並び、カーソルまで古いクエリのもので上書きされる。
  //
  // 開始時に採番し、**自分が最新でなければ何も書きません。**
  // 画面を離れるときも進めるので、外れたあとの更新も止まります。
  const generation = useRef(0);

  const hydrateThreads = useCallback(async (rs: Report[]) => {
    const ids = [...new Set(rs.map(threadIdOf))]
      .filter((id): id is number => id !== undefined)
      .filter((id) => !fetched.current.has(id));
    ids.forEach((id) => fetched.current.add(id));

    await Promise.all(
      ids.map(async (id) => {
        try {
          const t = await getThread(id);
          setThreads((prev) => ({ ...prev, [id]: t }));
        } catch (e) {
          // **404 は情報です。** 既に削除されていることを意味します。
          if (e instanceof ApiError && e.code === 'NOT_FOUND') {
            setThreads((prev) => ({ ...prev, [id]: 'missing' }));
            return;
          }
          // **それ以外は「取得できなかった」と出します** (レビュー指摘)。
          // 何も入れないと `undefined` のままになり、行は
          // 「取得しています...」を出し続けます。再取得を起こす導線は
          // 次のページか絞り込みの変更しかないので、**実際には
          // 永久に読み込み中に見えます。**
          //
          // `fetched` から外すのは、次に引くときに再試行するためです
          // (成功すればこの状態は上書きされます)。
          fetched.current.delete(id);
          setThreads((prev) => ({ ...prev, [id]: 'failed' }));
        }
      }),
    );
  }, []);

  const load = useCallback(
    async (opts: { cursor?: string; reset: boolean }) => {
      const gen = ++generation.current;
      const latest = () => gen === generation.current;

      setLoading(true);
      setError(null);
      try {
        const list = await listReports({
          status,
          cursor: opts.cursor,
          size: pageSize,
        });
        // **追い越されていたら 1 つも書きません。**
        // 一覧・カーソル・loading のどれか 1 つでも書くと、
        // 新しい取得の結果と混ざります。
        if (!latest()) return;

        setReports((prev) => (opts.reset ? list.reports : [...prev, ...list.reports]));
        setCursor(list.nextCursor);
        if (opts.reset) setResults({});
        await hydrateThreads(list.reports);
      } catch (e) {
        if (!latest()) return;
        setError(describe(e));
        // **失敗したら古い一覧を残しません。**
        // 絞り込みを変えた直後に失敗すると、見出しと中身が食い違います。
        if (opts.reset) {
          setReports([]);
          setCursor(null);
        }
      } finally {
        // 追い越されている場合、loading を落とすのは新しい取得の役目です。
        if (latest()) setLoading(false);
      }
    },
    [status, hydrateThreads],
  );

  // 絞り込みを変えたら先頭から引き直します。
  // `load` は status が変わったときだけ作り直されるので、
  // この効果もそのときだけ走ります。
  //
  // **一覧の破棄は load の中でやります** —— 効果の中で同期的に
  // setState すると、取得を待つ間だけ「0 件」が見える形になります。
  //
  // 効果の**同期部分では state を触りません** (react-hooks/set-state-in-effect)。
  // 1 回待ってから呼ぶことで、更新が描画の確定後になります ——
  // **描画の回数が減るわけではなく**、確定前の巻き戻しが無くなるだけです。
  // 片付けの `alive` は、待っている間に外れた場合に**開始そのもの**を
  // 止めます (開発時の Strict Mode では効果が 2 回走ります)。
  // 世代を進めるのは、**既に走っている取得**を無効にするためです ——
  // こちらが無いと、外れたあとに返った応答が書き込みます。
  useEffect(() => {
    let alive = true;
    void (async () => {
      await Promise.resolve();
      if (alive) await load({ reset: true });
    })();
    return () => {
      alive = false;
      generation.current += 1;
    };
  }, [load]);

  /** 通報の状態だけを変えます。**投稿には触れません。** */
  async function onResolve(report: Report, next: 'resolved' | 'rejected') {
    setRowBusy(report.id, true);
    try {
      const updated = await resolveReport(report.id, next);
      // 一覧から消さずに、その場で書き換えます ——
      // 押した直後に行が消えると、何をしたのかが確認できません。
      setReports((prev) => prev.map((r) => (r.id === report.id ? updated : r)));
      setResults((prev) => ({ ...prev, [report.id]: `${statusLabel[next]} として記録しました` }));
    } catch (e) {
      const msg =
        e instanceof ApiError && e.code === 'NOT_FOUND'
          ? '既に処理済みか、通報が存在しません'
          : describe(e);
      setResults((prev) => ({ ...prev, [report.id]: msg }));
    } finally {
      setRowBusy(report.id, false);
    }
  }

  /** 通報された投稿を削除します。**通報の状態は変わりません。** */
  async function onDelete(report: Report) {
    const threadID = threadIdOf(report);
    if (report.targetType === 'comment' && threadID === undefined) {
      // **起こらないはずの状態です** —— reports.target_thread_id は
      // CHECK 制約でコメントの通報に必須です。それでも 0 を捏造して
      // 送らないようにします。パーティションを絞れないだけでなく、
      // 「何を消したか」の記録が壊れます。
      setResults((prev) => ({
        ...prev,
        [report.id]: 'スレッド ID が無いため削除できません (通報の記録が壊れています)',
      }));
      return;
    }

    setRowBusy(report.id, true);
    try {
      const reason = reasons[report.id]?.trim();
      const recorded = await createModerationAction({
        // **対象種別から操作を導きます。** API は `action` しか受け取らず、
        // 対象種別はそこから決まります (食い違う組み合わせを作れないように)。
        action: report.targetType === 'thread' ? 'delete_thread' : 'delete_comment',
        targetId: String(report.targetId),
        ...(report.targetType === 'comment' ? { threadId: threadID } : {}),
        ...(reason ? { reason } : {}),
      });
      setResults((prev) => ({
        ...prev,
        [report.id]: `削除しました (記録 #${recorded.id} / 対象 ${recorded.targetId})`,
      }));
      if (report.targetType === 'thread') {
        setThreads((prev) => ({ ...prev, [report.targetId]: 'missing' }));
      }
    } catch (e) {
      const msg =
        e instanceof ApiError && e.code === 'NOT_FOUND'
          ? '既に削除されています'
          : describe(e);
      setResults((prev) => ({ ...prev, [report.id]: msg }));
    } finally {
      setRowBusy(report.id, false);
    }
  }

  return (
    <div>
      <div className="field">
        <label className="field__label" htmlFor="status">
          絞り込み
        </label>
        <select
          id="status"
          className="select"
          value={status}
          onChange={(e) => setStatus(e.target.value as ReportStatus)}
        >
          <option value="open">未処理</option>
          <option value="resolved">対処した</option>
          <option value="rejected">対処不要</option>
        </select>
        {/*
          **部分索引が効くのは open のときだけ**です (ADR 0011 決定 4)。
          解決済みは全体の走査になるので、調査用と割り切っています。
        */}
        {status !== 'open' && (
          <span className="field__hint">未処理以外は全走査になります (調査用)</span>
        )}
      </div>

      {error !== null && (
        <p className="alert alert--error" role="alert">
          {error}
        </p>
      )}

      {/*
        **失敗しているときは出しません** (レビュー指摘)。
        取得に失敗すると一覧を空にするので、赤いエラーの直下に
        「通報はありません」が並び、**API が落ちているのに
        「未処理は無い」と読める**表示になります。
      */}
      {reports.length === 0 && !loading && error === null && (
        <p className="muted">この状態の通報はありません。</p>
      )}

      <ul className="list">
        {reports.map((r) => {
          const threadID = threadIdOf(r);
          const thread = threadID === undefined ? undefined : threads[threadID];
          return (
            <li key={r.id} className="card">
              <p>
                #{r.id} / {r.targetType === 'thread' ? 'スレッド' : 'コメント'} {r.targetId} /{' '}
                {reasonLabel[r.reason]} / <span className="badge">{statusLabel[r.status]}</span>
              </p>
              <p className="meta">
                <time dateTime={r.createdAt}>{formatTime(r.createdAt)}</time>
                {r.resolvedAt !== null && <span>→ {formatTime(r.resolvedAt)}</span>}
              </p>

              {/* **通報者は出しません。** 誰が通報したかが見えると報復の材料になります。 */}
              {r.note !== null && <p className="body-text">補足: {r.note}</p>}

              <p className="muted">
                {/*
                  **threadID が無い行を「取得しています」で固めない**
                  (レビュー指摘)。hydrateThreads は undefined を除外するので、
                  この行の `threads[...]` は永久に埋まりません ——
                  onDelete 側 (「起こらないはずの状態です」) は同じ状態を
                  検出して文言を出しているのに、表示側だけ抜けていました。
                  123-127 行で `failed` を足して潰したのと同じ形になります。
                */}
                {threadID === undefined && '対象のスレッドが記録されていません'}
                {threadID !== undefined &&
                  thread === undefined &&
                  '対象のスレッドを取得しています...'}
                {thread === 'missing' && '対象のスレッドは既に削除されています'}
                {thread === 'failed' && '対象のスレッドを取得できませんでした'}
                {thread !== undefined && thread !== 'missing' && thread !== 'failed' && (
                  <>
                    スレッド:{' '}
                    {/*
                      **詳細へ繋ぎます。** コメントを 1 件だけ引く API は
                      無いままなので、コメントの通報では対象そのものではなく
                      「そのコメントがあるスレッド」へ飛びます。
                      数字だけを見て判断するよりは近づけます。
                    */}
                    <Link href={`/threads/${thread.id}`}>{thread.title}</Link>
                    {r.targetType === 'comment' && '（対象のコメント本文は未表示）'}
                  </>
                )}
              </p>

              <div className="field">
                <label className="field__label" htmlFor={`reason-${r.id}`}>
                  削除の理由 (任意・監査記録に残る)
                </label>
                <input
                  id={`reason-${r.id}`}
                  className="input"
                  value={reasons[r.id] ?? ''}
                  maxLength={500}
                  onChange={(e) => setReasons((prev) => ({ ...prev, [r.id]: e.target.value }))}
                />
              </div>

              <div className="actions">
                <button
                  type="button"
                  className="btn btn--danger"
                  disabled={busy.has(r.id)}
                  onClick={() => void onDelete(r)}
                >
                  対象を削除
                </button>
                <button
                  type="button"
                  className="btn"
                  disabled={busy.has(r.id) || r.status !== 'open'}
                  onClick={() => void onResolve(r, 'resolved')}
                >
                  対処した
                </button>
                <button
                  type="button"
                  className="btn"
                  disabled={busy.has(r.id) || r.status !== 'open'}
                  onClick={() => void onResolve(r, 'rejected')}
                >
                  対処不要
                </button>
              </div>

              {results[r.id] !== undefined && (
                <p className="alert alert--warn" role="status">
                  {results[r.id]}
                </p>
              )}
            </li>
          );
        })}
      </ul>

      {loading && (
        <p className="muted" aria-live="polite">
          読み込んでいます...
        </p>
      )}

      {/*
        **`nextCursor` をそのまま渡します** —— 中身は不透明で、
        クライアントは解釈も生成も改変もしません (ADR 0018)。
      */}
      {cursor !== null && !loading && (
        <div className="actions">
          <button type="button" className="btn" onClick={() => void load({ cursor, reset: false })}>
            次のページ
          </button>
        </div>
      )}
    </div>
  );
}
