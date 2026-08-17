import type { Metadata } from 'next';

import { NewThreadForm } from './NewThreadForm';

// スレッドを立てる画面。
//
// **この Server Component は利用者ごとの内容を 1 つも持ちません。**
// ログイン状態 (アイコンを付けられるか) は NewThreadForm がブラウザから
// 引きます —— ここで引くと ADR 0005 の 4 層キャッシュを全部
// 確認する必要が出ます。静的なまま配れるのは、中身が空だからです。
export const metadata: Metadata = { title: 'スレッドを立てる' };

export default function NewThreadPage() {
  return (
    <>
      <h1>スレッドを立てる</h1>
      <p className="muted measure">
        ログインしていなくても立てられます。ただし匿名で立てたスレッドは、
        あとから自分で削除できません (投稿者を特定する情報が残らないためです)。
      </p>
      <NewThreadForm />
    </>
  );
}
