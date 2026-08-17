import Link from 'next/link';

import type { components } from '../schema';

import { formatTime, machineTime } from './lib/ui';

// schema.d.ts は openapi.yaml から自動生成される (npm run gen:types)。
// ここで手書きの型を作らないことで、API とフロントの定義が必ず一致する。
type Thread = components['schemas']['Thread'];
type ThreadList = components['schemas']['ThreadList'];

/**
 * 取得結果。
 *
 * `rejected` は「絞り込みや位置を API が受け付けなかった」ことを表す。
 *
 * `effective` は**実際に使われた条件**になる。拒否されたときは
 * 引き直したほうの条件が入る —— 画面のリンクはすべてこちらから組み立てる。
 * 要求した条件のまま組み立てると、**「次のページへ」がまた 400 を踏んで
 * 1 ページ目に戻り、そこから抜けられなくなる** (レビュー指摘)。
 */
type FetchResult = { list: ThreadList; rejected: boolean; effective: Query };

/** 並び順。仕様書の `sort` パラメータと対になる。 */
type Sort = 'new' | 'popular';

/** 一覧の取得条件。URL の検索文字列とそのまま対応する。 */
type Query = { q: string; sort: Sort; cursor: string };

/**
 * この画面だけ **Server Component から API を叩いている**。
 *
 * 詳細 (app/threads/[id]) はブラウザから取っている ——
 * あちらは `GET /threads/{id}` が閲覧数を計上し、その識別子が
 * 未ログインだと IP になるため、サーバから叩くと
 * **Next のコンテナが全利用者を代表してしまう** (ADR 0006)。
 *
 * 一覧にはその問題が無く、
 *
 *   - 検索語と並び順が URL に載るので、**そのまま共有・ブックマークできる**
 *   - 最初の描画に本文が入るので、**JavaScript を待たずに読める**
 *
 * 側の利点が残る。**同じ経路で揃えないことを意図的に選んでいる。**
 */
async function fetchThreads({ q, sort, cursor }: Query): Promise<Response> {
  // Server Components はコンテナ内から叩くので、サーバ間通信用の API_URL を優先する。
  // NEXT_PUBLIC_ 接頭辞つきの変数はブラウザにも露出するため、
  // サーバ専用の宛先はそちらに入れない。
  const apiUrl =
    process.env.API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? 'http://localhost:8080';

  // **必ずエンコードする。** 検索語には & や # がそのまま入ってくる ——
  // 生で繋ぐと「& 以降が別のパラメータになる」形の壊れ方をする。
  const params = new URLSearchParams();
  if (q !== '') {
    params.set('q', q);
  }
  // **既定値は送らない。** 送っても結果は同じだが、URL が
  // `?sort=new` で埋まると「並び順を指定した状態」に見える。
  if (sort !== 'new') {
    params.set('sort', sort);
  }
  // **カーソルは不透明トークン** (ADR 0018)。中身を解釈も生成もせず、
  // 受け取った値をそのまま返すだけにする。
  if (cursor !== '') {
    params.set('cursor', cursor);
  }
  const search = params.size === 0 ? '' : `?${params.toString()}`;

  return fetch(`${apiUrl}/threads${search}`, { cache: 'no-store' });
}

/**
 * スレッド一覧を取得する。
 *
 * **400 で例外にしない** (レビュー指摘)。`q` と `cursor` は利用者が URL に
 * 直接書ける値なので、`?q=` に 201 文字を貼るだけで API が 400 を返す。
 * 例外にすると `app/` に `error.tsx` が無い以上、**掲示板が丸ごと
 * 表示できなくなる** —— 入力の誤りとしては代償が大きすぎる。
 * 絞り込みと位置を落として引き直し、断り書きを出す。
 *
 * `maxLength={200}` はフォーム経由しか守らないので、ここが最後の砦になる。
 *
 * **5xx は例外のまま。** そちらは利用者の入力ではなく API 側の障害で、
 * 一覧を出せる見込みが無い。握り潰すと壊れていることが伝わらない。
 */
async function getThreads(query: Query): Promise<FetchResult> {
  const res = await fetchThreads(query);
  if (res.ok) {
    return { list: (await res.json()) as ThreadList, rejected: false, effective: query };
  }

  if (res.status === 400 && (query.q !== '' || query.cursor !== '')) {
    // **並び順も一緒に落とす。** 原因が検索語とは限らない ——
    // 検索と人気順の同時指定も 400 になり、カーソルは並び順ごとに
    // 意味が変わるため、1 つずつ落とすと同じ 400 を何度も踏む。
    const fallback: Query = { q: '', sort: 'new', cursor: '' };
    const retry = await fetchThreads(fallback);
    if (retry.ok) {
      return { list: (await retry.json()) as ThreadList, rejected: true, effective: fallback };
    }
    throw new Error(`Failed to fetch threads: ${retry.status} ${retry.statusText}`);
  }

  throw new Error(`Failed to fetch threads: ${res.status} ${res.statusText}`);
}

/** 一覧の URL を組み立てる。**検索語と並び順を持ち回るのはここだけ。** */
function href({ q, sort, cursor }: Query): string {
  const params = new URLSearchParams();
  if (q !== '') params.set('q', q);
  if (sort !== 'new') params.set('sort', sort);
  if (cursor !== '') params.set('cursor', cursor);
  return params.size === 0 ? '/' : `/?${params.toString()}`;
}

function ThreadCard({ thread }: { thread: Thread }) {
  return (
    <li className="card thread">
      {thread.icon !== null && (
        // next/image を使っていない。配信元が環境ごとに変わり (MinIO / CDN)、
        // 最適化の経路に載せるには許可ホストを設定に固定する必要があるため
        // (ADR 0007 決定 5)。
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
        <h2 className="thread__title">
          <Link href={`/threads/${thread.id}`}>{thread.title}</Link>
        </h2>
        <p className="meta">
          <span>コメント {thread.commentCount}</span>
          {/*
            **閲覧数は概算** (ADR 0006)。計上はアプリのメモリ上で行い、
            一定間隔でまとめて反映するため、いま表示している値は
            最大でその間隔ぶん古い。「約」を付けているのは、
            更新直後に数字が動かないのを不具合と読まれないため。
          */}
          <span>閲覧数 約 {thread.viewCount}</span>
          <time dateTime={machineTime(thread.createdAt)}>{formatTime(thread.createdAt)}</time>
        </p>
      </div>
    </li>
  );
}

/**
 * 検索フォーム。
 *
 * **素の GET フォームで、JavaScript を使わない。**
 *
 *   - `method="get"` の送信はただのページ遷移なので、`csrfGuard` に
 *     関係しない (守っているのは状態変更メソッドだけ。ADR 0013 決定 1)。
 *     Server Action にすると POST になり、
 *     **サーバ側の `fetch` は `Origin` を送らないため 403 になる**
 *   - 検索語が URL に残るので、結果をそのまま共有・ブックマークできる。
 *     クライアント側の状態にすると URL と表示がずれる
 */
function SearchForm({ q, sort }: { q: string; sort: Sort }) {
  return (
    <form method="get" action="/" role="search" className="card">
      <div className="field">
        <label className="field__label" htmlFor="q">
          タイトルで探す
        </label>
        <input
          id="q"
          type="search"
          name="q"
          className="input"
          // **maxLength は仕様書の上限と同じ。** 超えると API が 400 を返す。
          maxLength={200}
          defaultValue={q}
          placeholder="キーワード"
        />
        <span className="field__hint">
          タイトルの中間一致で絞り込みます。コメント本文は検索できません。
        </span>
      </div>

      {/*
        **並び順は送らない。** 検索と人気順の同時指定は 400 になるため
        (索引をどちらか一方しか使えない)、検索したら新着順に戻る。
        `cursor` も送らない —— 絞り込みを変えたら位置は先頭に戻る。
      */}
      <div className="actions">
        <button type="submit" className="btn">
          検索
        </button>
        {q !== '' && (
          <Link className="btn btn--quiet" href={href({ q: '', sort, cursor: '' })}>
            絞り込みを解除
          </Link>
        )}
      </div>
    </form>
  );
}

/**
 * 並び替え。
 *
 * **検索フォームと同じく、素の GET リンク。**
 * クライアント側の状態にすると URL と表示がずれ、
 * 並び順を含めた結果を共有できなくなる。
 *
 * **検索中は人気順を出さない。** API は `q` と `sort=popular` の
 * 同時指定を 400 で弾く (索引をどちらか一方しか使えないため)。
 * ここで選べてしまうと、押した瞬間にエラーになる操作を見せることになる。
 */
function SortLinks({ q, sort }: { q: string; sort: Sort }) {
  if (q !== '') {
    return <p className="muted">検索結果は新着順で表示しています。</p>;
  }

  return (
    <nav className="actions" aria-label="並び順">
      {/*
        **`aria-current` の値は `page` に揃える** (レビュー指摘)。
        これもページ遷移で、ヘッダのナビと種類が同じになる。
        `true` と `page` が混ざると、CSS の当たり方も 2 系統になる。
      */}
      <Link
        className="btn"
        href={href({ q, sort: 'new', cursor: '' })}
        aria-current={sort === 'new' ? 'page' : undefined}
      >
        新着順
      </Link>
      <Link
        className="btn"
        href={href({ q, sort: 'popular', cursor: '' })}
        aria-current={sort === 'popular' ? 'page' : undefined}
      >
        人気順
      </Link>
    </nav>
  );
}

/**
 * `?sort=` を並び順に解釈する。
 *
 * **未知の値は既定 (新着順) に落とす。** API 側は 400 にするが、
 * こちらで落とすのは「画面が丸ごと出せなくなる」のを避けるためで、
 * `?q=` を 400 で例外にしないのと同じ判断になる。
 * 落とした結果は画面の「新着順」が選択状態になることで見える。
 */
function toSort(raw: string | string[] | undefined): Sort {
  const value = Array.isArray(raw) ? raw[0] : raw;
  return value === 'popular' ? 'popular' : 'new';
}

/**
 * 同じ名前が複数回来ることがある (`?q=a&q=b`)。フォームからは起きないが、
 * URL は手で組み立てられる。配列のまま API へ渡すと
 * `q=a,b` のような値になるので、先頭だけを使う。
 */
function first(raw: string | string[] | undefined): string {
  if (Array.isArray(raw)) {
    return raw[0] ?? '';
  }
  return raw ?? '';
}

export default async function Page({
  searchParams,
}: {
  // Next.js 15 以降、searchParams は Promise で渡る。
  searchParams: Promise<{ [key: string]: string | string[] | undefined }>;
}) {
  const params = await searchParams;
  const query: Query = {
    q: first(params.q),
    sort: toSort(params.sort),
    // **中身を検査しない。** 不透明トークンなので、形を知っているのは
    // サーバだけになる (ADR 0018)。壊れた値は 400 で返り、
    // 上の `getThreads` が絞り込みごと落として引き直す。
    cursor: first(params.cursor),
  };
  const {
    list: { threads, nextCursor },
    rejected,
    // **要求した条件ではなく、実際に使われた条件で画面を組む** (レビュー指摘)。
    // 拒否された `q` を持ち回ると、「次のページへ」がまた 400 を踏んで
    // 1 ページ目へ戻り、**そこから抜けられなくなる。**
    // 検索欄と並び順の表示が実際の結果と食い違うのも同じ理由になる。
    effective,
  } = await getThreads(query);

  return (
    <>
      <h1>スレッド一覧</h1>

      <div className="actions">
        <Link className="btn btn--primary" href="/threads/new">
          スレッドを立てる
        </Link>
      </div>

      <SearchForm q={effective.q} sort={effective.sort} />

      <SortLinks q={effective.q} sort={effective.sort} />

      {rejected && (
        <p className="alert alert--warn" role="status">
          この条件は受け付けられませんでした (検索語が長すぎるか、ページの位置が
          古くなっています)。絞り込みなしの先頭ページを表示しています。
        </p>
      )}

      {threads.length === 0 ? (
        // **「見つからない」と「まだ無い」を区別する。**
        // 検索して 0 件のときに「スレッドがまだありません」と出ると、
        // 掲示板が空だと読めてしまう。
        <p className="muted">
          {/*
            **`effective` を見る。** 拒否された条件では絞り込んでいないので、
            「一致しません」と出すと検索語が使われたように読める。
          */}
          {effective.q === ''
            ? 'スレッドがまだありません。最初のスレッドを立ててみてください。'
            : `「${effective.q}」に一致するスレッドはありません。`}
        </p>
      ) : (
        <ul className="list">
          {threads.map((thread) => (
            <ThreadCard key={thread.id} thread={thread} />
          ))}
        </ul>
      )}

      {/*
        **「前のページ」は作らない。** カーソルは不透明で前方向にしか進めず、
        戻る位置を作るには、辿ってきたカーソルを URL に積むことになる
        (ADR 0018)。ブラウザの戻るで足りる範囲なので、先頭へ戻る導線だけ置く。

        **中身が無いときは要素ごと出さない** (レビュー指摘) ——
        空のランドマークが、支援技術のランドマーク一覧に並ぶ。
      */}
      {(nextCursor !== null || effective.cursor !== '') && (
        <nav className="actions" aria-label="ページ送り">
          {nextCursor !== null && (
            <Link className="btn" href={href({ ...effective, cursor: nextCursor })}>
              次のページへ
            </Link>
          )}
          {effective.cursor !== '' && (
            <Link className="btn btn--quiet" href={href({ ...effective, cursor: '' })}>
              先頭に戻る
            </Link>
          )}
        </nav>
      )}
    </>
  );
}
