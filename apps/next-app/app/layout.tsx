import React from 'react';

export const metadata = {
  title: 'Develop Experiments',
  description: 'ワイヤードと繋がる掲示板',
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="ja">
      <body style={{ backgroundColor: '#0d0f12', color: '#e2e8f0', fontFamily: 'monospace', padding: '2rem' }}>
        {children}
      </body>
    </html>
  );
}