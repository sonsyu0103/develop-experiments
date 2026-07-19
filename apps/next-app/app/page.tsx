import React from 'react';
import type { components } from '../schema';

type ThreadDTO = components['schemas']['ThreadDTO'];

// APIのレスポンス全体の型定義（ { threads: ThreadDTO[] } の形 ）
type ApiResponse = {
  threads: ThreadDTO[];
};

async function getThreads(): Promise<ThreadDTO[]> {
  // 環境変数があればそれを使う（Docker用）、なければ localhost（ローカル用）
  const apiUrl = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080';
  
  const res = await fetch(`${apiUrl}/threads`, { cache: 'no-store' });
  if (!res.ok) {
    throw new Error('Failed to fetch threads from wired network');
  }

  // レンスポンスを一度オブジェクトとして受け取る
  const data = (await res.json()) as ApiResponse;

  // もしデータや threads が存在しない場合の安全弁をつけて、配列を返す
  return data?.threads || [];
}

export default async function Page() {
  const threads = await getThreads();

  return (
    <div style={{ backgroundColor: '#000', color: '#00f', minHeight: '100vh', padding: '2rem', fontFamily: 'monospace' }}>
      <h1 style={{ borderBottom: '1px solid #00f', paddingBottom: '0.5rem' }}>Wired Thread List</h1>
      <ul style={{ listStyle: 'none', padding: 0 }}>
        {threads.map((thread) => (
          <li key={thread.id} style={{ margin: '1rem 0', padding: '1rem', border: '1px dashed #00f' }}>
            <h2 style={{ fontSize: '1.2rem', margin: '0 0 0.5rem 0' }}>{thread.title}</h2>
            <p style={{ color: '#55f', margin: 0 }}>コメント数: {thread.commentCount}</p>
          </li>
        ))}
      </ul>
    </div>
  );
}