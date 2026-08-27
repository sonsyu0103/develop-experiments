import type { Metadata } from 'next';

import { ContactForm } from './ContactForm';

export const metadata: Metadata = { title: 'お問い合わせ' };

// 問い合わせ画面 (docs/adr/0008-contact-and-mail.md)。
//
// **この Server Component は利用者ごとの内容を 1 つも持ちません。**
// ログイン済みの初期値は ContactForm がブラウザから引きます ——
// ここで引くと ADR 0005 の 4 層キャッシュを全部確認する必要が出ます。
// 静的なまま配れるのは、中身が空だからです。
export default function ContactPage() {
  return (
    <>
      <h1>お問い合わせ</h1>

      {/*
        **ログインを求めません** (ADR 0008 決定 4)。
        「ログインできない」という問い合わせが来る以上、
        必須にすると詰みます。ログイン済みなら初期値が埋まるだけです。
      */}
      <p className="muted measure">
        ログインしていなくても送れます。ログイン中の場合は、お名前と
        メールアドレスが初期値として埋まります。
      </p>

      <ContactForm />
    </>
  );
}
