'use client';

// 管理画面への導線。**moderator 以上にだけ出します。**
//
// `Me.role` はこのために返っています —— 他人のロールは `Author` に
// 含めていません (誰がモデレーターかを一覧で晒す必要がないため)。
//
// **これは防御ではありません。** リンクを隠しても /admin は開けますし、
// 開いたところで API が 403 を返します。押せないものを見せないだけです。
//
// 【なぜ Client Component なのか】
// ここでロールを取りに行くとページが利用者ごとの内容を持つことになり、
// ADR 0005 の 4 層キャッシュを全部確認する必要が出ます。
// **サーバが返す HTML には何も入れず**、ブラウザから引きます。
import { useEffect, useState } from 'react';
import Link from 'next/link';

import { getMe } from './lib/api';

export function AdminLink() {
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    let alive = true;
    getMe()
      .then((me) => {
        if (alive) setVisible(me.role !== 'user');
      })
      // **未ログインは例外ではありません。** 401 が既定の状態なので、
      // ここでは黙って出さないだけにします。
      .catch(() => undefined);
    return () => {
      alive = false;
    };
  }, []);

  if (!visible) return null;

  return (
    <p style={{ margin: '0 0 1rem' }}>
      <Link href="/admin" style={{ color: '#55f' }}>
        通報キューを開く
      </Link>
    </p>
  );
}
