'use client';

// 通報 (ADR 0011 決定 4)。スレッドとコメントの両方から使います。
//
// 【ログインが要ります。投稿とは非対称です】
// 投稿は匿名を許すのに通報は許さないのは、**投稿が表現で、
// 通報が他人への申し立て**だからです。匿名で受け付けると、
// 通報そのものがキューを埋める荒らしの手段になります。
//
// 【2 回目は成功でも失敗でもありません】
// 同じ人が同じ対象を通報すると、API は 200 で**最初の通報**を返します
// (一意制約。優先度を操作できないようにするため)。エラーではないので
// 「通報しました」と出して構いませんが、返ってきた `status` が
// `open` でない場合は**既に処理済みで、キューには積まれていません。**
// そこだけは伝えないと、対処されたことに気づかないまま再送を繰り返します。
import { useId, useState } from 'react';

import {
  createReport,
  loginUrl,
  type ReportReason,
  type ReportTargetType,
} from '../lib/api';
import { describeWriteError } from '../lib/errors';

/** 理由は**自由記述ではなく固定の 4 値**です (集計と絞り込みのため)。 */
const reasons: ReadonlyArray<{ value: ReportReason; label: string }> = [
  { value: 'spam', label: 'スパム・宣伝' },
  { value: 'abuse', label: '誹謗中傷・嫌がらせ' },
  { value: 'illegal', label: '違法な内容' },
  { value: 'other', label: 'その他' },
];

/** 補足の上限。**DB の CHECK 制約 `reports_note_length` と同じ値です。** */
const noteLimit = 1000;

type Phase =
  | { kind: 'closed' }
  | { kind: 'open' }
  | { kind: 'sending' }
  | { kind: 'done'; alreadyHandled: boolean }
  | { kind: 'error'; message: string };

type Props = {
  targetType: ReportTargetType;
  targetId: number;
  /** `targetType` が `comment` のときだけ必要です (パーティションキー)。 */
  threadId?: number;
  /** 未ログインなら、フォームではなくログインの案内を出します。 */
  signedIn: boolean;
};

export function ReportForm({ targetType, targetId, threadId, signedIn }: Props) {
  const reasonId = useId();
  const noteId = useId();
  const [phase, setPhase] = useState<Phase>({ kind: 'closed' });
  const [reason, setReason] = useState<ReportReason>('spam');
  const [note, setNote] = useState('');

  async function onSubmit() {
    setPhase({ kind: 'sending' });
    try {
      const report = await createReport({
        targetType,
        targetId,
        // **スレッドの通報では送れません** (送ると 400)。
        ...(targetType === 'comment' && threadId !== undefined ? { threadId } : {}),
        reason,
        ...(note.trim() === '' ? {} : { note: note.trim() }),
      });
      setNote('');
      setPhase({ kind: 'done', alreadyHandled: report.status !== 'open' });
    } catch (e) {
      setPhase({ kind: 'error', message: describeWriteError(e) });
    }
  }

  if (phase.kind === 'done') {
    return (
      <p className="alert alert--ok" role="status">
        通報しました。
        {phase.alreadyHandled &&
          ' なお、この対象への通報は既に処理済みです (新しくキューには積まれていません)。'}
      </p>
    );
  }

  if (phase.kind === 'closed') {
    return (
      <button
        type="button"
        className="btn btn--quiet"
        aria-expanded={false}
        onClick={() => setPhase({ kind: 'open' })}
      >
        通報する
      </button>
    );
  }

  if (!signedIn) {
    return (
      <div className="card">
        <p>通報にはログインが必要です。</p>
        <p className="muted">
          投稿は匿名でもできますが、通報は他人への申し立てなので、
          責任の所在が分かる形にしています。
        </p>
        {/*
          **Next.js の <Link> ではありません。** 遷移先は Go API の 302 で、
          クライアント側ルーティングでは辿れません。
        */}
        <a className="btn" href={loginUrl()}>
          Google でログインする
        </a>
        <button type="button" className="btn btn--quiet" onClick={() => setPhase({ kind: 'closed' })}>
          やめる
        </button>
      </div>
    );
  }

  const sending = phase.kind === 'sending';

  return (
    <div className="card">
      <div className="field">
        <label className="field__label" htmlFor={reasonId}>
          通報の理由
        </label>
        <select
          id={reasonId}
          className="select"
          value={reason}
          disabled={sending}
          onChange={(e) => setReason(e.target.value as ReportReason)}
        >
          {reasons.map((r) => (
            <option key={r.value} value={r.value}>
              {r.label}
            </option>
          ))}
        </select>
      </div>

      <div className="field">
        <label className="field__label" htmlFor={noteId}>
          補足 (任意)
        </label>
        <textarea
          id={noteId}
          className="textarea"
          value={note}
          maxLength={noteLimit}
          disabled={sending}
          onChange={(e) => setNote(e.target.value)}
        />
        <span className="field__hint">
          {note.length} / {noteLimit} 文字
        </span>
      </div>

      <div className="row">
        <button type="button" className="btn" disabled={sending} onClick={() => void onSubmit()}>
          {sending ? '送信しています...' : '通報を送る'}
        </button>
        <button
          type="button"
          className="btn btn--quiet"
          disabled={sending}
          onClick={() => setPhase({ kind: 'closed' })}
        >
          やめる
        </button>
      </div>

      {phase.kind === 'error' && (
        <p className="alert alert--error" role="alert">
          {phase.message}
        </p>
      )}
    </div>
  );
}
