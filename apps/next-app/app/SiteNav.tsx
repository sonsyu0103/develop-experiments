'use client';

// ヘッダの画面間ナビ。
//
// 【なぜ Client Component なのか】
// 2 つあります。
//
//   - **いまどの画面にいるか**を出すのに `usePathname()` が要る
//   - **管理の導線をロールで出し分ける。** ここでロールを引くことで、
//     サーバが返す HTML と RSC ペイロードは誰に対しても同じままになります
//     (ADR 0005 の 4 層キャッシュが fetch 1 か所に減る)
//
// 【これは防御ではありません】
// リンクを隠しても `/admin` は開けますし、開いても API が 403 を返します。
// **押せないものを見せない**だけの部品です。
import Link from 'next/link';
import { usePathname } from 'next/navigation';

import { useMe } from './lib/me';

/** 常に出る行き先。 */
const items = [
  { href: '/', label: 'スレッド一覧' },
  { href: '/threads/new', label: 'スレッドを立てる' },
  { href: '/mypage', label: 'マイページ' },
  { href: '/contact', label: 'お問い合わせ' },
] as const;

/**
 * いま開いている画面かどうか。
 *
 * **トップだけは完全一致にします。** 前方一致にすると、
 * すべての画面でトップが選択状態になります。
 */
function isCurrent(pathname: string, href: string): boolean {
  return href === '/' ? pathname === '/' : pathname.startsWith(href);
}

export function SiteNav() {
  const pathname = usePathname();
  const { state } = useMe();

  // **リストにしません** (`<ul><li>`)。行き先が 5 つ以下で、
  // 読み上げ時に「リスト 5 項目」と前置きされるほうが冗長になります。
  return (
    <nav className="site-nav" aria-label="画面の切り替え">
      {items.map((item) => (
        <Link
          key={item.href}
          href={item.href}
          // 見た目 (下線) は globals.css が持ちます。
          // こちらは支援技術向けの表明で、**どちらか一方では足りません。**
          aria-current={isCurrent(pathname, item.href) ? 'page' : undefined}
        >
          {item.label}
        </Link>
      ))}

      {/*
        **moderator 以上にだけ出します。** `Me.role` はこのために返っています
        —— 他人のロールは `Author` に含めていません
        (誰がモデレーターかを一覧で晒す必要がないため)。

        **未ログイン (401) は失敗ではありません。** 既定の状態なので、
        黙って出さないだけにします。
      */}
      {state.kind === 'ready' && state.me.role !== 'user' && (
        <Link href="/admin" aria-current={isCurrent(pathname, '/admin') ? 'page' : undefined}>
          管理
        </Link>
      )}
    </nav>
  );
}
