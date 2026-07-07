import React from 'react';
// { components } の前に「type」を明示してあげるの
import type { components } from '../schema';

type ThreadDTO = components['schemas']['ThreadDTO'];

async function getThreads(): Promise<ThreadDTO[]> {
  // Goのバックエンドサーバー（8080番ポート）に繋ぎにいく
  // cache: 'no-store' をつけて、常に最新の並列集計データを取得するの
  const res = await fetch('http://localhost:8080/threads', { cache: 'no-store' });
  
  if (!res.ok) {
    throw new Error('ワイヤードからの応答がありません。');
  }
  
  const data = await res.json();
  return data.threads;
}

export default async function Page() {
  const threads = await getThreads();

  return (
    <div>
      <h1 style={{ borderBottom: '1px solid #334155', paddingBottom: '0.5rem', color: '#38bdf8' }}>
        Wired Thread List
      </h1>
      <div style={{ marginTop: '2rem', display: 'flex', flexDirection: 'column', gap: '1rem' }}>
        {threads.map((thread) => (
          <div 
            key={thread.id} 
            style={{ border: '1px solid #1e293b', padding: '1rem', borderRadius: '4px', backgroundColor: '#111827' }}
          >
            <h2 style={{ margin: 0, fontSize: '1.25rem' }}>{thread.title}</h2>
            <p style={{ margin: '0.5rem 0 0 0', color: '#94a3b8', fontSize: '0.875rem' }}>
              コメント数: <span style={{ color: '#f43f5e', fontWeight: 'bold' }}>{thread.commentCount}</span>
            </p>
          </div>
        ))}
      </div>
    </div>
  );
}