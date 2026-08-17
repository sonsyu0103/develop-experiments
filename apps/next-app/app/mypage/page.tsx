import type { Metadata } from 'next';

import { MyProfile } from './MyProfile';

// マイページ。
//
// **この Server Component は利用者ごとの内容を 1 つも持ちません。**
// 中身は MyProfile がブラウザから引きます —— ここで引くと、
// この画面だけ ADR 0005 の 4 層キャッシュ (fetch / レンダリング /
// Router Cache / CDN) を全部確認する必要が出ます。
export const metadata: Metadata = { title: 'マイページ' };

export default function MyPage() {
  return (
    <>
      <h1>マイページ</h1>
      <MyProfile />
    </>
  );
}
