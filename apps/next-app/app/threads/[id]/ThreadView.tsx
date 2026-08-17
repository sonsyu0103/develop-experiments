'use client';

// スレッド詳細の本体。取得も更新もすべてブラウザから行います
// (理由は同じディレクトリの page.tsx)。
//
// 【この画面で気をつけている状態遷移】
// ADR 0020 が挙げた 4 つの落とし穴は、そのままここにも当てはまります。
//
//   - **古い応答が新しい一覧に混ざる。** 「更新」を押した直後に、
//     押す前の「もっと読む」が返ってくる。世代番号で捨てます
//   - **削除の二重送信。** 進行中の印を 1 つの ID で持つと、
//     行をまたいだときに素通りします。集合で持ちます
//   - **一過性の失敗が「読み込み中」で固定される。** 取得の失敗は
//     必ず表示できる状態へ落とし、再試行の導線を出します
//   - **エラーと「まだありません」の同時表示。** 取得に失敗しただけの
//     ものを「コメントはありません」と書くと、空のスレッドに見えます
import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { useCallback, useEffect, useRef, useState } from 'react';

import {
  deleteComment,
  deleteThread,
  getThread,
  listComments,
  loginUrl,
  type Author,
  type Comment,
  type Me,
  type Thread,
} from '../../lib/api';
import { describe, describeWriteError, isCode } from '../../lib/errors';
import { useMe } from '../../lib/me';
import { formatTime, machineTime } from '../../lib/ui';
import { Attachment } from '../../components/Attachment';
import { ReportForm } from '../../components/ReportForm';

import { CommentForm } from './CommentForm';

type ThreadState =
  | { kind: 'loading' }
  | { kind: 'ready'; thread: Thread }
  // **404 は「エラー」と分けます。** 消えたスレッドに再試行の導線を出すと、
  // 何度押しても同じ結果になるものを押させることになります。
  | { kind: 'missing' }
  | { kind: 'error'; message: string };

type CommentsState = { kind: 'loading' } | { kind: 'ready' } | { kind: 'error'; message: string };

/**
 * 自分の投稿か。
 *
 * **退会済みは対象外です。** その場合 `publicId` はフィールドごと
 * 省略されるので (`withdrawn` の分岐を落としても他人の投稿を
 * 自分のものと判定しないための設計)、比較そのものが成立しません。
 */
function isMine(author: Author, me: Me | null): boolean {
  if (me === null || author === null || author.publicId === undefined) {
    // 匿名投稿 (`author` が null) と退会済み (`publicId` が無い) はここで落ちます。
    return false;
  }
  return author.publicId === me.publicId;
}

export function ThreadView({ threadId }: { threadId: number }) {
  const router = useRouter();
  const { state: meState } = useMe();
  const me = meState.kind === 'ready' ? meState.me : null;
  const signedIn = me !== null;
  // **ログイン状態が決まるまで、投稿ごとの操作を出しません** (レビュー指摘)。
  // 決まる前は `me` が null なので、自分の投稿にも「通報する」が出て、
  // 一拍あとに「削除」へ化けます —— 押そうとした先が変わるのは事故のもとになります。
  const meResolved = meState.kind !== 'loading';

  const [threadState, setThreadState] = useState<ThreadState>({ kind: 'loading' });
  const [commentsState, setCommentsState] = useState<CommentsState>({ kind: 'loading' });
  const [comments, setComments] = useState<Comment[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const [moreError, setMoreError] = useState<string | null>(null);

  // 削除の進行中と、行ごとの失敗。**単一の ID では足りません** ——
  // 行をまたいで押されたときに、片方の状態がもう片方を消します (ADR 0020 ②)。
  const [deletingIds, setDeletingIds] = useState<ReadonlySet<number>>(new Set());
  const [rowErrors, setRowErrors] = useState<ReadonlyMap<number, string>>(new Map());
  const [confirmingId, setConfirmingId] = useState<number | null>(null);
  const [threadDelete, setThreadDelete] = useState<
    { kind: 'idle' } | { kind: 'confirming' } | { kind: 'deleting' } | { kind: 'error'; message: string }
  >({ kind: 'idle' });

  // 取得の世代。**読み込み直すたびに進めます。**
  // 遅れて届いた前の世代の応答は、ここで捨てます。
  const generation = useRef(0);

  const load = useCallback(() => {
    const gen = ++generation.current;

    setThreadState({ kind: 'loading' });
    setCommentsState({ kind: 'loading' });
    setComments([]);
    setNextCursor(null);
    setMoreError(null);
    setRowErrors(new Map());
    // **進行中の印も戻す** (レビュー指摘)。世代が変わった時点で
    // 前の「もっと読む」は捨てるので、その完了を待つ理由がない ——
    // 戻さないと、新しい一覧に続きがあってもボタンが
    // 「取得しています...」のまま押せない。
    setLoadingMore(false);

    getThread(threadId)
      .then((thread) => {
        if (gen !== generation.current) return;
        setThreadState({ kind: 'ready', thread });
      })
      .catch((e: unknown) => {
        if (gen !== generation.current) return;
        setThreadState(
          isCode(e, 'NOT_FOUND') ? { kind: 'missing' } : { kind: 'error', message: describe(e) },
        );
      });

    listComments(threadId)
      .then((page) => {
        if (gen !== generation.current) return;
        setComments(page.comments);
        setNextCursor(page.nextCursor);
        setCommentsState({ kind: 'ready' });
      })
      .catch((e: unknown) => {
        if (gen !== generation.current) return;
        // **「読み込み中」のままにしません。** 一過性の失敗が
        // 永久に読み込み中として固定されると、再試行の手段がなくなります。
        setCommentsState({ kind: 'error', message: describe(e) });
      });
  }, [threadId]);

  useEffect(load, [load]);

  async function loadMore() {
    if (nextCursor === null || loadingMore) return;

    const gen = generation.current;
    setLoadingMore(true);
    setMoreError(null);
    try {
      const page = await listComments(threadId, { cursor: nextCursor });
      // **世代が変わっていたら捨てます。** 「更新」を押した直後に
      // 前の続きが届くと、新しい一覧に古い続きが混ざります (ADR 0020 ①)。
      if (gen !== generation.current) return;
      setComments((prev) => merge(prev, page.comments));
      setNextCursor(page.nextCursor);
    } catch (e) {
      if (gen !== generation.current) return;
      setMoreError(describe(e));
    } finally {
      // 世代が変わっている場合、この印は `load` が既に戻している。
      if (gen === generation.current) setLoadingMore(false);
    }
  }

  function onPosted(posted: Comment) {
    // 一覧は**新しい順**なので、投稿は先頭に入ります。
    setComments((prev) => merge([posted], prev));
    setThreadState((s) =>
      s.kind === 'ready'
        ? { kind: 'ready', thread: { ...s.thread, commentCount: s.thread.commentCount + 1 } }
        : s,
    );
  }

  async function onDeleteComment(comment: Comment) {
    setConfirmingId(null);
    setDeletingIds((prev) => new Set(prev).add(comment.id));
    setRowErrors((prev) => without(prev, comment.id));
    try {
      await deleteComment(threadId, comment.id);
      setComments((prev) => prev.filter((c) => c.id !== comment.id));
      setThreadState((s) =>
        s.kind === 'ready'
          ? {
              kind: 'ready',
              // **0 を下回らせません。** 件数は取得時点の値なので、
              // 表示中に他の人が消していると計算がずれます。
              thread: { ...s.thread, commentCount: Math.max(0, s.thread.commentCount - 1) },
            }
          : s,
      );
    } catch (e) {
      // **404 は「既に消えている」なので、行も消します** (レビュー指摘)。
      // 別の端末やモデレーターが先に消した場合がこれに当たります。
      // 残すと、何度押しても 404 になる行が画面に居座ります。
      if (isCode(e, 'NOT_FOUND')) {
        setComments((prev) => prev.filter((c) => c.id !== comment.id));
        return;
      }
      setRowErrors((prev) => new Map(prev).set(comment.id, describeWriteError(e)));
    } finally {
      setDeletingIds((prev) => {
        const next = new Set(prev);
        next.delete(comment.id);
        return next;
      });
    }
  }

  async function onDeleteThread() {
    setThreadDelete({ kind: 'deleting' });
    try {
      await deleteThread(threadId);
      // 一覧へ戻します。**`refresh()` も呼びます** —— 一覧は
      // Server Component なので、これが無いと消したスレッドが
      // クライアント側の控えから出続けます。
      router.push('/');
      router.refresh();
    } catch (e) {
      setThreadDelete({ kind: 'error', message: describeWriteError(e) });
    }
  }

  if (threadState.kind === 'missing') {
    return (
      <>
        <h1>スレッドが見つかりません</h1>
        <p className="muted">
          削除されたか、URL が違っています。削除されたスレッドは一覧からも取得からも消えます。
        </p>
        <p>
          <Link href="/">スレッド一覧へ</Link>
        </p>
      </>
    );
  }

  if (threadState.kind === 'error') {
    return (
      <>
        <h1>スレッドを表示できません</h1>
        <p className="alert alert--error" role="alert">
          {threadState.message}
        </p>
        <button type="button" className="btn" onClick={load}>
          再試行する
        </button>
      </>
    );
  }

  if (threadState.kind === 'loading') {
    return (
      <>
        <h1>読み込んでいます...</h1>
        <p className="muted" aria-live="polite">
          スレッドを取得しています。
        </p>
      </>
    );
  }

  const thread = threadState.thread;
  const ownThread = isMine(thread.author, me);

  return (
    <>
      <article className="card">
        <div className="thread">
          {thread.icon !== null && (
            // eslint-disable-next-line @next/next/no-img-element
            <img
              className="thread__icon"
              src={thread.icon.url}
              width={thread.icon.width}
              height={thread.icon.height}
              alt=""
            />
          )}
          <div>
            <h1>{thread.title}</h1>
            <p className="meta">
              <Byline author={thread.author} />
              <time dateTime={machineTime(thread.createdAt)}>{formatTime(thread.createdAt)}</time>
              <span>コメント {thread.commentCount}</span>
              {/*
                **「約」を外しません。** 計上はアプリのメモリ上で行い、
                一定間隔でまとめて反映するので、いま出している値は
                最大でその間隔ぶん古くなります (ADR 0006)。
                更新直後に数字が動かないのを不具合と読まれないための表記です。
              */}
              <span>閲覧数 約 {thread.viewCount}</span>
            </p>
          </div>
        </div>

        <div className="actions">
          {meResolved && ownThread && threadDelete.kind !== 'confirming' && (
            <button
              type="button"
              className="btn btn--danger"
              disabled={threadDelete.kind === 'deleting'}
              onClick={() => setThreadDelete({ kind: 'confirming' })}
            >
              {threadDelete.kind === 'deleting' ? '削除しています...' : 'このスレッドを削除'}
            </button>
          )}

          {/*
            **自分の投稿には通報を出しません。** 自分で消せるものを
            モデレーターのキューに積む理由がなく、押せば積まれてしまいます。
          */}
          {meResolved && !ownThread && (
            <ReportForm targetType="thread" targetId={thread.id} signedIn={signedIn} />
          )}
        </div>

        {threadDelete.kind === 'confirming' && (
          <div className="alert alert--warn">
            {/*
              **確認を挟みます。** 論理削除なので DB からは戻せますが、
              戻す画面はありません (ADR 0011 の引き受けたコスト)。
              コメントは消えず、添付画像もストレージに残ります。
            */}
            <p>このスレッドを削除します。元に戻す画面はありません。</p>
            <p className="muted">
              コメントは個別には消えず、スレッドごと見えなくなります。添付画像はストレージに残ります
              (消すのはモデレーターの操作です)。
            </p>
            <div className="actions">
              <button type="button" className="btn btn--danger" onClick={() => void onDeleteThread()}>
                削除する
              </button>
              <button
                type="button"
                className="btn btn--quiet"
                onClick={() => setThreadDelete({ kind: 'idle' })}
              >
                やめる
              </button>
            </div>
          </div>
        )}

        {threadDelete.kind === 'error' && (
          <p className="alert alert--error" role="alert">
            {threadDelete.message}
          </p>
        )}
      </article>

      <h2>コメントを書く</h2>
      {/*
        **フォームもログイン状態が決まってから出します。**
        先に出すと、名前欄と画像欄と案内文が、決まった瞬間に入れ替わります ——
        書き始めた欄が消えるのは、操作を取り違えさせる形になります。
        コメントの読み取りは待たせないので、遅れるのはここだけです。
      */}
      {!meResolved ? (
        <p className="muted" aria-live="polite">
          ログイン状態を確認しています...
        </p>
      ) : (
        <>
          {!signedIn && (
            <p className="muted">
              ログインしなくても投稿できます。
              <a href={loginUrl()}>Google でログイン</a>
              すると、画像の添付と、自分の投稿の削除ができます。
            </p>
          )}
          <CommentForm threadId={threadId} signedIn={signedIn} onPosted={onPosted} />
        </>
      )}

      <div className="section-head">
        <h2>コメント {thread.commentCount} 件</h2>
        {/*
          **自動では更新しません。** 一定間隔で引き直すと、
          読んでいる最中に行がずれます。押したときだけ取り直します。
        */}
        <button type="button" className="btn btn--quiet" onClick={load}>
          最新の状態にする
        </button>
      </div>
      <p className="muted">新しい順に表示しています。番号 (&gt;&gt;) は削除しても再利用されません。</p>

      {commentsState.kind === 'loading' && (
        <p className="muted" aria-live="polite">
          コメントを取得しています...
        </p>
      )}

      {commentsState.kind === 'error' && (
        <div className="alert alert--error" role="alert">
          <p>コメントを取得できませんでした。</p>
          <p className="muted">{commentsState.message}</p>
          <div className="actions">
            <button type="button" className="btn" onClick={load}>
              再試行する
            </button>
          </div>
        </div>
      )}

      {/*
        **取得に失敗したときは「ありません」と書きません** (ADR 0020 ④)。
        投稿が消えたように見えます。
      */}
      {commentsState.kind === 'ready' && comments.length === 0 && (
        <p className="muted">まだコメントはありません。</p>
      )}

      {comments.length > 0 && (
        <ul className="list">
          {comments.map((comment) => (
            <li className="comment" key={comment.id}>
              <p className="meta">
                <span className="comment__seq">&gt;&gt;{comment.seq}</span>
                <Byline author={comment.author} fallbackName={comment.authorName} />
                <time dateTime={machineTime(comment.createdAt)}>{formatTime(comment.createdAt)}</time>
              </p>

              <p className="body-text">{comment.body}</p>

              {comment.image !== null && (
                <Attachment image={comment.image} alt={`>>${comment.seq} の添付画像`} />
              )}

              {/*
                **ログイン状態が決まるまで出しません。** 決まる前に出すと、
                自分の投稿にも「通報する」が並び、一拍あとに「削除」へ化けます。
              */}
              <div className="actions">
                {!meResolved ? null : isMine(comment.author, me) ? (
                  confirmingId === comment.id ? (
                    <>
                      <span className="muted">このコメントを削除しますか?</span>
                      <button
                        type="button"
                        className="btn btn--danger"
                        onClick={() => void onDeleteComment(comment)}
                      >
                        削除する
                      </button>
                      <button
                        type="button"
                        className="btn btn--quiet"
                        onClick={() => setConfirmingId(null)}
                      >
                        やめる
                      </button>
                    </>
                  ) : (
                    <button
                      type="button"
                      className="btn btn--quiet"
                      // **行ごとに見ます。** 全体で 1 つの印にすると、
                      // 別の行の削除中に、この行の削除が通ります。
                      disabled={deletingIds.has(comment.id)}
                      onClick={() => setConfirmingId(comment.id)}
                    >
                      {deletingIds.has(comment.id) ? '削除しています...' : '削除'}
                    </button>
                  )
                ) : (
                  <ReportForm
                    targetType="comment"
                    targetId={comment.id}
                    // **パーティションキーです。** これが無いと、
                    // 通報を受けた側が 8 パーティションすべてを走査します。
                    threadId={threadId}
                    signedIn={signedIn}
                  />
                )}
              </div>

              {rowErrors.has(comment.id) && (
                <p className="alert alert--error" role="alert">
                  {rowErrors.get(comment.id)}
                </p>
              )}
            </li>
          ))}
        </ul>
      )}

      {moreError !== null && (
        <p className="alert alert--error" role="alert">
          続きを取得できませんでした: {moreError}
        </p>
      )}

      {nextCursor !== null && commentsState.kind === 'ready' && (
        <div className="actions">
          <button type="button" className="btn" disabled={loadingMore} onClick={() => void loadMore()}>
            {loadingMore ? '取得しています...' : 'もっと読む'}
          </button>
        </div>
      )}
    </>
  );
}

/**
 * 投稿者の表示。
 *
 * **`author` があるときは、そちらを優先します** (ADR 0014)。
 * ログイン中の投稿では `authorName` は既定値のまま保存されるため、
 * こちらを先に見ないと、全員が「名無しさん」になります。
 */
function Byline({ author, fallbackName }: { author: Author; fallbackName?: string }) {
  if (author === null) {
    return (
      <span>{fallbackName !== undefined && fallbackName !== '' ? fallbackName : '名無しさん'}</span>
    );
  }

  return (
    <span className="row">
      {author.avatarUrl !== undefined && author.avatarUrl !== null && (
        // eslint-disable-next-line @next/next/no-img-element
        <img className="avatar avatar--sm" src={author.avatarUrl} width={24} height={24} alt="" />
      )}
      {/* 退会済みは API 側で「退会したユーザー」に置き換わっています。 */}
      <span>{author.displayName}</span>
    </span>
  );
}

/**
 * コメントを重複なく繋ぎます。
 *
 * 新着順 + カーソルなので普段は重複しませんが、
 * **投稿した直後に「もっと読む」を押した場合**など、
 * 同じ行が 2 度現れうる経路が残ります。`key` が重複すると
 * React が更新の対象を取り違えるので、ここで潰します。
 */
function merge(head: Comment[], tail: Comment[]): Comment[] {
  const seen = new Set(head.map((c) => c.id));
  return [...head, ...tail.filter((c) => !seen.has(c.id))];
}

/** Map から 1 件外した新しい Map を返します。 */
function without(map: ReadonlyMap<number, string>, key: number): ReadonlyMap<number, string> {
  const next = new Map(map);
  next.delete(key);
  return next;
}
