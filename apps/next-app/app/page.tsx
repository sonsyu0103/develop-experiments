import Link from 'next/link';

import type { components } from '../schema';

import { AdminLink } from './AdminLink';

// schema.d.ts は openapi.yaml から自動生成される (npm run gen:types)。
// ここで手書きの型を作らないことで、API とフロントの定義が必ず一致する。
type Thread = components['schemas']['Thread'];
type ThreadList = components['schemas']['ThreadList'];

/** 取得結果。`rejected` は「検索語を API が受け付けなかった」ことを表す。 */
type FetchResult = { list: ThreadList; rejected: boolean };

/** 並び順。仕様書の `sort` パラメータと対になる。 */
type Sort = 'new' | 'popular';

async function fetchThreads(query: string, sort: Sort): Promise<Response> {
  // Server Components はコンテナ内から叩くので、サーバ間通信用の API_URL を優先する。
  // NEXT_PUBLIC_ 接頭辞つきの変数はブラウザにも露出するため、
  // サーバ専用の宛先はそちらに入れない。
  const apiUrl =
    process.env.API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? 'http://localhost:8080';

  // **必ずエンコードする。** 検索語には & や # がそのまま入ってくる ——
  // 生で繋ぐと「& 以降が別のパラメータになる」形の壊れ方をする。
  const params = new URLSearchParams();
  if (query !== '') {
    params.set('q', query);
  }
  // **既定値は送らない。** 送っても結果は同じだが、URL が
  // `?sort=new` で埋まると「並び順を指定した状態」に見える。
  if (sort !== 'new') {
    params.set('sort', sort);
  }
  const search = params.size === 0 ? '' : `?${params.toString()}`;

  return fetch(`${apiUrl}/threads${search}`, { cache: 'no-store' });
}

/**
 * スレッド一覧を取得する。
 *
 * **400 で例外にしない** (レビュー指摘)。`q` は利用者が URL に直接書ける
 * 唯一の値なので、`?q=` に 201 文字を貼るだけで API が 400 を返す。
 * 例外にすると `app/` に `error.tsx` が無い以上、**掲示板が丸ごと
 * 表示できなくなる** —— 入力の誤りとしては代償が大きすぎる。
 * 検索語を落として引き直し、絞り込みなしの一覧と断り書きを出す。
 *
 * `maxLength={200}` はフォーム経由しか守らないので、ここが最後の砦になる。
 *
 * **5xx は例外のまま。** そちらは利用者の入力ではなく API 側の障害で、
 * 一覧を出せる見込みが無い。握り潰すと壊れていることが伝わらない。
 */
async function getThreads(query: string, sort: Sort): Promise<FetchResult> {
  const res = await fetchThreads(query, sort);
  if (res.ok) {
    return { list: (await res.json()) as ThreadList, rejected: false };
  }

  if (res.status === 400 && query !== '') {
    // **並び順も一緒に落とす。** 検索語が原因とは限らない ——
    // 検索と人気順の同時指定も 400 になるため、`q` だけ落として
    // 引き直すと同じ 400 をもう一度踏むことがある。
    const retry = await fetchThreads('', 'new');
    if (retry.ok) {
      return { list: (await retry.json()) as ThreadList, rejected: true };
    }
    throw new Error(`Failed to fetch threads: ${retry.status} ${retry.statusText}`);
  }

  throw new Error(`Failed to fetch threads: ${res.status} ${res.statusText}`);
}

// Server Component なので、日時の整形はコンテナのタイムゾーンで行われる。
// compose で TZ を指定していない環境では UTC になり、9 時間ずれる。
// 実行環境に依存させないよう timeZone を明示する。
function ThreadCard({ thread }: { thread: Thread }) {
  return (
    <li style={{ margin: '1rem 0', padding: '1rem', border: '1px dashed #00f' }}>
      <h2 style={{ fontSize: '1.2rem', margin: '0 0 0.5rem 0' }}>{thread.title}</h2>
      <p style={{ color: '#55f', margin: 0 }}>
        コメント数: {thread.commentCount}
        {/*
          **閲覧数は概算です** (docs/adr/0006-view-count-and-popularity.md)。
          計上はアプリのメモリ上で行い、一定間隔でまとめて反映するため、
          いま表示している値は最大でその間隔ぶん古い。
          「約」を付けているのは、更新直後に数字が動かないのを
          不具合と読まれないため。
        */}
        <span style={{ marginLeft: '1rem' }}>閲覧数: 約 {thread.viewCount}</span>
        <span style={{ marginLeft: '1rem', opacity: 0.7 }}>
          {new Date(thread.createdAt).toLocaleString('ja-JP', {
            timeZone: 'Asia/Tokyo',
          })}
        </span>
      </p>
    </li>
  );
}

/**
 * 検索フォーム。
 *
 * **素の GET フォームで、JavaScript を使いません。**
 *
 *   - `method="get"` の送信はただのページ遷移なので、`csrfGuard` に
 *     関係しない (守っているのは状態変更メソッドだけ。ADR 0013 決定 1)。
 *     Server Action にすると POST になり、
 *     **サーバ側の `fetch` は `Origin` を送らないため 403 になる**
 *   - 検索語が URL に残るので、結果をそのまま共有・ブックマークできる。
 *     クライアント側の状態にすると URL と表示がずれる
 *
 * 管理画面をブラウザから叩いている (`app/lib/api.ts`) のと逆向きに見えるが、
 * 理由は同じ **「Origin が要るかどうか」** になる。
 * こちらは読み取りの GET なので、サーバから取ってよい。
 */
function SearchForm({ query }: { query: string }) {
  return (
    <form method="get" action="/" style={{ margin: '1rem 0' }}>
      <label htmlFor="q" style={{ marginRight: '0.5rem' }}>
        タイトル検索
      </label>
      <input
        id="q"
        type="search"
        name="q"
        // **maxLength は仕様書の上限と同じ。** 超えると API が 400 を返す。
        maxLength={200}
        defaultValue={query}
        placeholder="キーワード"
        style={{
          backgroundColor: '#000',
          color: '#00f',
          border: '1px solid #00f',
          fontFamily: 'monospace',
          padding: '0.25rem 0.5rem',
        }}
      />
      <button
        type="submit"
        style={{
          backgroundColor: 'transparent',
          color: '#00f',
          border: '1px solid #00f',
          fontFamily: 'monospace',
          padding: '0.25rem 0.75rem',
          marginLeft: '0.5rem',
          cursor: 'pointer',
        }}
      >
        検索
      </button>
      {query !== '' && (
        <Link href="/" style={{ color: '#55f', marginLeft: '1rem' }}>
          解除
        </Link>
      )}
    </form>
  );
}

/**
 * 並び替え。
 *
 * **検索フォームと同じく、素の GET リンクです。**
 * クライアント側の状態にすると URL と表示がずれ、
 * 並び順を含めた結果を共有できなくなります。
 *
 * **検索中は人気順を出しません。** API は `q` と `sort=popular` の
 * 同時指定を 400 で弾きます (索引をどちらか一方しか使えないため。
 * 仕様書の Sort パラメータ)。ここで選べてしまうと、
 * 押した瞬間にエラーになる操作を見せることになります。
 */
function SortLinks({ query, sort }: { query: string; sort: Sort }) {
  if (query !== '') {
    return (
      <p style={{ color: '#55f', opacity: 0.7, margin: '0 0 1rem 0' }}>
        検索結果は新着順で表示しています。
      </p>
    );
  }

  const linkStyle = (active: boolean) => ({
    color: active ? '#000' : '#00f',
    backgroundColor: active ? '#00f' : 'transparent',
    border: '1px solid #00f',
    padding: '0.25rem 0.75rem',
    marginRight: '0.5rem',
    textDecoration: 'none',
  });

  return (
    <p style={{ margin: '0 0 1rem 0' }}>
      <Link href="/" style={linkStyle(sort === 'new')}>
        新着順
      </Link>
      <Link href="/?sort=popular" style={linkStyle(sort === 'popular')}>
        人気順
      </Link>
    </p>
  );
}

/**
 * `?sort=` を並び順に解釈します。
 *
 * **未知の値は既定 (新着順) に落とします。** API 側は 400 にしますが、
 * こちらで落とすのは「画面が丸ごと出せなくなる」のを避けるためで、
 * `?q=` を 400 で例外にしないのと同じ判断になります。
 * 落とした結果は画面の「新着順」が選択状態になることで見えます。
 */
function toSort(raw: string | string[] | undefined): Sort {
  const value = Array.isArray(raw) ? raw[0] : raw;
  return value === 'popular' ? 'popular' : 'new';
}

/**
 * `?q=` を 1 つの文字列にします。
 *
 * **同じ名前が複数回来ることがある** (`?q=a&q=b`)。フォームからは起きないが、
 * URL は手で組み立てられる。配列のまま API へ渡すと
 * `q=a,b` のような検索語になるので、先頭だけを使う。
 */
function toQuery(raw: string | string[] | undefined): string {
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
  const query = toQuery(params.q);
  const sort = toSort(params.sort);
  const {
    list: { threads, nextCursor },
    rejected,
  } = await getThreads(query, sort);

  return (
    <div
      style={{
        backgroundColor: '#000',
        color: '#00f',
        minHeight: '100vh',
        padding: '2rem',
        fontFamily: 'monospace',
      }}
    >
      <h1 style={{ borderBottom: '1px solid #00f', paddingBottom: '0.5rem' }}>
        Wired Thread List
      </h1>

      {/*
        **Client Component です。** ロールはブラウザから引きます ——
        ここで引くと、この Server Component の出力が利用者ごとに変わり、
        ADR 0005 の 4 層キャッシュを全部確認する必要が出ます。
      */}
      <AdminLink />

      <SearchForm query={query} />

      <SortLinks query={query} sort={sort} />

      {rejected && (
        <p style={{ color: '#fa0' }}>
          この検索語は受け付けられませんでした (長すぎるか、使えない文字が
          含まれています)。絞り込みなしの一覧を表示しています。
        </p>
      )}

      {threads.length === 0 ? (
        // **「見つからない」と「まだ無い」を区別する。**
        // 検索して 0 件のときに「スレッドがまだありません」と出ると、
        // 掲示板が空だと読めてしまう。
        <p style={{ color: '#55f' }}>
          {/*
            **rejected のときは絞り込んでいない。**
            「一致しません」と出すと、検索語が使われたように読める。
          */}
          {query === '' || rejected
            ? 'スレッドがまだありません。'
            : `「${query}」に一致するスレッドはありません。`}
        </p>
      ) : (
        <ul style={{ listStyle: 'none', padding: 0 }}>
          {threads.map((thread) => (
            <ThreadCard key={thread.id} thread={thread} />
          ))}
        </ul>
      )}

      {nextCursor !== null && (
        <p style={{ color: '#55f', opacity: 0.7 }}>
          次ページのカーソル: {nextCursor}
        </p>
      )}
    </div>
  );
}
