import type { Metadata } from 'next';
import { notFound } from 'next/navigation';

import { ThreadView } from './ThreadView';

// スレッド詳細。
//
// 【この Server Component は API を叩きません】
// 一覧 (app/page.tsx) はサーバから取っているのに、ここだけブラウザから
// 取ります。逆に見えますが、理由は 3 つあり、どれもこの画面に固有です。
//
//   - **閲覧数が壊れます。** `GET /threads/{id}` は閲覧を計上し、
//     その識別子は未ログインだと `ip:` になります
//     (apps/go-api/internal/httpapi/server.go の `visitorKey`)。
//     サーバから取ると **Next のコンテナの IP が全利用者を代表する**ため、
//     ADR 0006 の重複抑制に全員がまとめて掛かります
//   - **書き込みがどのみちブラウザからです** (ADR 0013)。コメント投稿・
//     削除・通報がすべて同じ画面にあるので、読みだけサーバに分けると
//     経路が 2 本になります
//   - **状態遷移を検査できます。** ADR 0020 の枠組み (`page.route`) は
//     ブラウザの要求しか差し替えられないので、サーバ取得だと
//     この画面は検査の外に出ます
//
// 【タイトルを `generateMetadata` で出していない理由も同じです】
// スレッド名をタブに出すにはサーバ側で 1 回取る必要があり、
// それは上の 1 つ目にそのまま当たります。
export const metadata: Metadata = { title: 'スレッド' };

/**
 * パスの ID を検査します。
 *
 * **`Number()` に直接渡しません。** `/threads/1e3` が 1000 になり、
 * `/threads/007` が 7 になります —— どちらも「別のスレッドが開く」形の
 * 壊れ方で、URL を共有したときに気づきにくくなります。
 */
function toThreadId(raw: string): number | null {
  if (!/^[1-9][0-9]*$/.test(raw)) return null;
  const id = Number(raw);
  return Number.isSafeInteger(id) ? id : null;
}

export default async function ThreadPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const threadId = toThreadId(id);
  if (threadId === null) {
    notFound();
  }

  return <ThreadView threadId={threadId} />;
}
