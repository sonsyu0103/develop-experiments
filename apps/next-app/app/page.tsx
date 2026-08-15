import type { components } from '../schema';

import { AdminLink } from './AdminLink';

// schema.d.ts は openapi.yaml から自動生成される (npm run gen:types)。
// ここで手書きの型を作らないことで、API とフロントの定義が必ず一致する。
type Thread = components['schemas']['Thread'];
type ThreadList = components['schemas']['ThreadList'];

async function getThreads(): Promise<ThreadList> {
  // Server Components はコンテナ内から叩くので、サーバ間通信用の API_URL を優先する。
  // NEXT_PUBLIC_ 接頭辞つきの変数はブラウザにも露出するため、
  // サーバ専用の宛先はそちらに入れない。
  const apiUrl =
    process.env.API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? 'http://localhost:8080';

  const res = await fetch(`${apiUrl}/threads`, { cache: 'no-store' });
  if (!res.ok) {
    throw new Error(`Failed to fetch threads: ${res.status} ${res.statusText}`);
  }

  return (await res.json()) as ThreadList;
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
        <span style={{ marginLeft: '1rem', opacity: 0.7 }}>
          {new Date(thread.createdAt).toLocaleString('ja-JP', {
            timeZone: 'Asia/Tokyo',
          })}
        </span>
      </p>
    </li>
  );
}

export default async function Page() {
  const { threads, nextCursor } = await getThreads();

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

      {threads.length === 0 ? (
        <p style={{ color: '#55f' }}>スレッドがまだありません。</p>
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
