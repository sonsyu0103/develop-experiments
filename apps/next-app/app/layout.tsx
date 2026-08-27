// Next.js 16 は React の automatic runtime を使うため、
// JSX を書くための `import React` は不要。型だけを import する。
import type { ReactNode } from 'react';
import type { Metadata } from 'next';
import Link from 'next/link';

// 見た目はここから 1 度だけ読み込む (app/globals.css)。
// **画面ごとに読み込まない** —— 読み込む順で優先度が変わり、
// どの指定が効くかが画面によって違う、という追いにくい壊れ方をする。
import './globals.css';
import { SiteNav } from './SiteNav';

export const metadata: Metadata = {
  title: {
    // 各画面が `title` を出すと「スレッドのタイトル | Develop Experiments」になる。
    // **タブを並べたときに見分けられる**ことのほうが、短さより効く。
    template: '%s | Develop Experiments',
    default: 'Develop Experiments',
  },
  description: 'ワイヤードと繋がる掲示板',
};

/**
 * 全画面の外枠。
 *
 * **利用者ごとの内容を 1 つも持ちません。** ヘッダの管理リンクだけは
 * ロールで出し分けますが、それは `SiteNav` (Client Component) が
 * ブラウザから引きます —— ここで引くと、すべての画面が
 * 利用者ごとの出力を持つことになり、ADR 0005 の 4 層キャッシュを
 * 全部確認する必要が出ます。
 */
export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="ja">
      <body>
        <div className="shell">
          {/*
            **Tab の 1 打鍵目にだけ現れるリンク。**
            これが無いと、キーボードだけで操作する利用者は
            画面を移るたびにヘッダのナビを読み飛ばすことになる (WCAG 2.4.1)。
          */}
          <a className="skip-link" href="#main">
            本文へ移動
          </a>

          <header className="site-header">
            <div className="site-header__inner">
              <p className="site-title">
                <Link href="/">Develop Experiments — Wired BBS</Link>
              </p>
              <SiteNav />
            </div>
          </header>

          {/*
            `id` はスキップリンクの着地点。`tabIndex={-1}` を付けているのは、
            **飛んだ先に焦点を移すため** —— 付けないと画面はスクロールするのに
            焦点はヘッダに残り、次の Tab でナビへ戻ってしまう。
          */}
          <main className="page" id="main" tabIndex={-1}>
            {children}
          </main>

          <footer className="site-footer">
            <div className="site-footer__inner">
              <p>
                並行制御と運用の実験用に作っている掲示板です。
                閲覧数は概算で、最新の値ではありません。
              </p>
            </div>
          </footer>
        </div>
      </body>
    </html>
  );
}
