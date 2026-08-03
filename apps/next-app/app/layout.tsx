// Next.js 16 は React の automatic runtime を使うため、
// JSX を書くための `import React` は不要。型だけを import する。
import type { ReactNode } from 'react';
import type { Metadata } from 'next';

export const metadata: Metadata = {
  title: 'Develop Experiments',
  description: 'ワイヤードと繋がる掲示板',
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="ja">
      <body
        style={{
          backgroundColor: '#0d0f12',
          color: '#e2e8f0',
          fontFamily: 'monospace',
          padding: '2rem',
        }}
      >
        {children}
      </body>
    </html>
  );
}
