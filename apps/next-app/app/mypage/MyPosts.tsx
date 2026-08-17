'use client';

// マイページの「自分の投稿」。
//
// 【この画面が引き受けている前提】
//
//   - **匿名で投稿したものは出ません。** 投稿時にログインしていなければ
//     `author_id` が入らず、後から本人だと突き合わせられません
//     (ADR 0005 決定 2)。**黙って省くと「消えた」と読まれる**ので、
//     一覧の脇に必ず添えます
//   - **削除済みスレッドへのコメントも出ます** (`threadDeleted`)。
//     落とすと「消えた」のか「元から無い」のかを本人が区別できません。
//     ただしリンクにはしません —— 押した先は 404 です
//   - コメント側の取得は `comments` の 8 区画すべてを走ります
//     (分割キーが `thread_id` で、`author_id` に含まれないため。ADR 0016)。
//     実測 1.5 ms で一覧としては問題にならないと判断しています
//
// 【状態遷移は ADR 0020 の 4 つをそのまま踏襲します】
// スレッド詳細 (ThreadView) と同じ落とし穴がここにも当てはまります ——
// 古い応答の混入・二重取得・失敗の固定・「ありません」の誤表示。
// 2 つの一覧が同じ振る舞いを要るので、`usePagedList` に括り出しました。
import Link from 'next/link';
import { useCallback, useEffect, useRef, useState } from 'react';

import {
  listMyComments,
  listMyThreads,
  type MyComment,
  type Thread,
} from '../lib/api';
import { describe } from '../lib/errors';
import { formatTime, machineTime } from '../lib/ui';
import { Attachment } from '../components/Attachment';

/** 一覧の取得状況。**items とは分けます** —— 続きの取得中も本体は出したままにするため。 */
type Status = { kind: 'loading' } | { kind: 'ready' } | { kind: 'error'; message: string };

type Page<T> = { items: T[]; nextCursor: string | null };

/**
 * カーソル方式の一覧を 1 本ぶん抱えます。
 *
 * **`enabled` が false のあいだは取りに行きません。**
 *
 * `fetchPage` は **`useCallback` で固定してください。** 毎回新しい関数を
 * 渡すと、効果が繰り返し走って取得が止まりません。
 */
function usePagedList<T>(
  fetchPage: (cursor?: string) => Promise<Page<T>>,
  enabled: boolean,
) {
  // **初期値が 'loading' であること自体に意味があります。** 開かれるまで
  // 取りに行かないので、最初の取得で status を触る必要がありません
  // (効果の中で同期的に setState しないための足場です。下記)。
  const [status, setStatus] = useState<Status>({ kind: 'loading' });
  const [items, setItems] = useState<T[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const [moreError, setMoreError] = useState<string | null>(null);

  // 取得の世代。読み込み直すたびに進め、遅れて届いた前の世代は捨てます。
  const generation = useRef(0);
  // 一度でも取りに行ったか。**state ではなく ref です。**
  // 描画に影響しない覚え書きなので state にする必要が無く、
  // state にすると効果の中で同期的に setState することになります
  // (react-hooks/set-state-in-effect。連鎖描画を招くため lint が止めます)。
  //
  // **`items.length` では代用できません** —— 「まだ取っていない」と
  // 「取ったが 0 件」が区別できず、投稿が無い人はタブを開くたびに
  // 取り直すことになります。
  const requested = useRef(false);

  /** 取得して詰めます。**同期的な状態変更を含みません** (効果から呼ぶため)。 */
  const run = useCallback(
    (gen: number) => {
      fetchPage()
        .then((page) => {
          if (gen !== generation.current) return;
          setItems(page.items);
          setNextCursor(page.nextCursor);
          setStatus({ kind: 'ready' });
        })
        .catch((e: unknown) => {
          if (gen !== generation.current) return;
          // **「読み込み中」のままにしません** (ADR 0020 ③)。
          // 一過性の失敗が永久に読み込み中で固定されると、再試行できません。
          setStatus({ kind: 'error', message: describe(e) });
        });
    },
    [fetchPage],
  );

  /** 先頭から取り直します。**押されたときにだけ呼びます** (効果からは呼びません)。 */
  const load = useCallback(() => {
    requested.current = true;
    setStatus({ kind: 'loading' });
    setItems([]);
    setNextCursor(null);
    setMoreError(null);
    // **進行中の印も戻します。** 世代が変わった時点で前の「もっと読む」は
    // 捨てるので、その完了を待つ理由がありません。戻さないと、
    // 新しい一覧に続きがあってもボタンが押せないままになります。
    setLoadingMore(false);
    run(++generation.current);
  }, [run]);

  useEffect(() => {
    // **開かれるまで取りに行きません。** 切り替えるまで開かれない側まで
    // 取ると、マイページを開くだけで 2 往復します。
    // 一度取ったものは切り替えても捨てません (戻すたびに取り直すと、
    // 読み進めた位置が毎回先頭に戻ります)。
    if (!enabled || requested.current) return;
    requested.current = true;
    run(++generation.current);
  }, [enabled, run]);

  async function loadMore() {
    if (nextCursor === null || loadingMore) return;

    const gen = generation.current;
    setLoadingMore(true);
    setMoreError(null);
    try {
      const page = await fetchPage(nextCursor);
      // **世代が変わっていたら捨てます** (ADR 0020 ①)。
      // 「取り直す」を押した直後に前の続きが届くと、新しい一覧に混ざります。
      if (gen !== generation.current) return;
      setItems((prev) => [...prev, ...page.items]);
      setNextCursor(page.nextCursor);
    } catch (e) {
      if (gen !== generation.current) return;
      setMoreError(describe(e));
    } finally {
      // 世代が変わっている場合、この印は load が既に戻しています。
      if (gen === generation.current) setLoadingMore(false);
    }
  }

  return { status, items, nextCursor, loadingMore, moreError, load, loadMore };
}

type Tab = 'threads' | 'comments';

export function MyPosts() {
  const [tab, setTab] = useState<Tab>('threads');

  // **`useCallback` で固定します。** 毎回新しい関数だと usePagedList の
  // 効果が回り続けます。`listMy*` はモジュール直下の定数なので依存は空です。
  const fetchThreads = useCallback(
    async (cursor?: string): Promise<Page<Thread>> => {
      const page = await listMyThreads({ cursor });
      return { items: page.threads, nextCursor: page.nextCursor };
    },
    [],
  );
  const fetchComments = useCallback(
    async (cursor?: string): Promise<Page<MyComment>> => {
      const page = await listMyComments({ cursor });
      return { items: page.comments, nextCursor: page.nextCursor };
    },
    [],
  );

  const threads = usePagedList(fetchThreads, tab === 'threads');
  const comments = usePagedList(fetchComments, tab === 'comments');
  const active = tab === 'threads' ? threads : comments;

  return (
    <>
      <div className="section-head">
        <h2>自分の投稿</h2>
        <button type="button" className="btn btn--quiet" onClick={active.load}>
          取り直す
        </button>
      </div>

      <p className="muted measure">
        {/*
          **匿名の投稿が出ないことを必ず書きます。** 出ないのは仕様ですが、
          画面に何も書かないと「消えた」と読まれます。
        */}
        新しい順に表示しています。ログインせずに投稿したものは、
        本人だと確かめる手立てがないため出てきません。
      </p>

      {/*
        **`role="tablist"` にはしていません。** その役割を名乗ると
        矢印キーでの移動が期待されますが、実装していないためです。
        押せば切り替わる 2 つのボタンとして、`aria-pressed` で状態を伝えます。
      */}
      <div className="actions">
        <TabButton current={tab} value="threads" onSelect={setTab}>
          立てたスレッド
        </TabButton>
        <TabButton current={tab} value="comments" onSelect={setTab}>
          書いたコメント
        </TabButton>
      </div>

      {active.status.kind === 'loading' && (
        <p className="muted" aria-live="polite">
          取得しています...
        </p>
      )}

      {active.status.kind === 'error' && (
        <div className="alert alert--error" role="alert">
          <p>投稿を取得できませんでした。</p>
          <p className="muted">{active.status.message}</p>
          <div className="actions">
            <button type="button" className="btn" onClick={active.load}>
              再試行する
            </button>
          </div>
        </div>
      )}

      {/*
        **取得に失敗したときは「ありません」と書きません** (ADR 0020 ④)。
        投稿が消えたように見えます。`status` が ready のときだけ出します。
      */}
      {tab === 'threads' && threads.status.kind === 'ready' && (
        threads.items.length === 0 ? (
          <p className="muted">まだスレッドを立てていません。</p>
        ) : (
          <ul className="list">
            {threads.items.map((thread) => (
              <MyThreadRow key={thread.id} thread={thread} />
            ))}
          </ul>
        )
      )}

      {tab === 'comments' && comments.status.kind === 'ready' && (
        comments.items.length === 0 ? (
          <p className="muted">まだコメントを書いていません。</p>
        ) : (
          <ul className="list">
            {comments.items.map((comment) => (
              <MyCommentRow key={comment.id} comment={comment} />
            ))}
          </ul>
        )
      )}

      {active.moreError !== null && (
        <p className="alert alert--error" role="alert">
          続きを取得できませんでした: {active.moreError}
        </p>
      )}

      {active.nextCursor !== null && active.status.kind === 'ready' && (
        <div className="actions">
          <button
            type="button"
            className="btn"
            disabled={active.loadingMore}
            onClick={() => void active.loadMore()}
          >
            {active.loadingMore ? '取得しています...' : 'もっと読む'}
          </button>
        </div>
      )}
    </>
  );
}

function TabButton({
  current,
  value,
  onSelect,
  children,
}: {
  current: Tab;
  value: Tab;
  onSelect: (tab: Tab) => void;
  children: React.ReactNode;
}) {
  const selected = current === value;
  return (
    <button
      type="button"
      className={selected ? 'btn btn--primary' : 'btn btn--quiet'}
      aria-pressed={selected}
      onClick={() => onSelect(value)}
    >
      {children}
    </button>
  );
}

/** 自分が立てたスレッド 1 件。一覧のカードと同じ形にしています。 */
function MyThreadRow({ thread }: { thread: Thread }) {
  return (
    <li className="card thread">
      {thread.icon !== null && (
        // next/image を使っていません (ADR 0007 決定 5。一覧と同じ理由)。
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
        <h3 className="thread__title">
          <Link href={`/threads/${thread.id}`}>{thread.title}</Link>
        </h3>
        <p className="meta">
          <span>コメント {thread.commentCount}</span>
          {/* 閲覧数は概算です (ADR 0006)。「約」を外しません。 */}
          <span>閲覧数 約 {thread.viewCount}</span>
          <time dateTime={machineTime(thread.createdAt)}>{formatTime(thread.createdAt)}</time>
        </p>
      </div>
    </li>
  );
}

/**
 * 自分が書いたコメント 1 件。
 *
 * **投稿者は出しません** —— 全部自分なので、行ごとに並べても意味がありません。
 * 代わりに「どのスレッドへの投稿か」を出します。
 */
function MyCommentRow({ comment }: { comment: MyComment }) {
  return (
    <li className="comment">
      <p className="meta">
        <span className="comment__seq">&gt;&gt;{comment.seq}</span>
        {/*
          **削除済みスレッドはリンクにしません。** 押した先は 404 です。
          タイトルは伏せません —— 伏せると、自分が何に書いたのか
          本人にも分からなくなります。
        */}
        {comment.threadDeleted ? (
          <span>{comment.threadTitle}</span>
        ) : (
          <Link href={`/threads/${comment.threadId}`}>{comment.threadTitle}</Link>
        )}
        <time dateTime={machineTime(comment.createdAt)}>{formatTime(comment.createdAt)}</time>
      </p>

      {comment.threadDeleted && (
        <p className="alert alert--warn">
          このスレッドは削除されています。コメントは残っていますが、開けません。
        </p>
      )}

      <p className="body-text">{comment.body}</p>

      {comment.image !== null && (
        <Attachment image={comment.image} alt={`>>${comment.seq} の添付画像`} />
      )}
    </li>
  );
}
