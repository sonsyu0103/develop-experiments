'use client';

// 問い合わせフォーム (docs/adr/0008-contact-and-mail.md)。
//
// 【なぜ Client Component なのか】
// 2 つあります。
//
//   - **書き込みはブラウザから直接叩く** (ADR 0013 の「実装して分かったこと 7」)。
//     Server Actions から叩くと `Origin` が付かず、csrfGuard に 403 で弾かれます
//   - **ログイン済みの初期値を引くため。** サーバ側で引くと、この画面の
//     出力が利用者ごとに変わり、ADR 0005 の 4 層キャッシュを全部
//     確認する必要が出ます (AdminLink と同じ理由)
//
// 【この画面が「送信しました」と言わない理由】
// API が返すのは `202 Accepted` で、**受理までしか終わっていません。**
// メールは定期処理が後から送ります。ここで「送信しました」と書くと、
// 送れなかった場合に利用者は成功したと思ったままになります。
import { useEffect, useState } from 'react';

import { ApiError, submitContact } from '../lib/api';
import { loadMe } from '../lib/me';

// **仕様書の maxLength と同じ値です** (api/openapi.yaml の CreateContactRequest)。
// ここは入力の途中で気づけるようにするためのもので、検査の正は API 側
// (さらに DB の CHECK 制約) にあります。
const limits = { name: 100, email: 254, subject: 200, body: 5000 } as const;

/** 送信の状態。**`sent` ではなく `accepted`** —— 送信は終わっていません。 */
type Phase = { kind: 'editing' } | { kind: 'sending' } | { kind: 'accepted' } | { kind: 'error'; message: string };

export function ContactForm() {
  const [name, setName] = useState('');
  const [email, setEmail] = useState('');
  const [subject, setSubject] = useState('');
  const [body, setBody] = useState('');
  // honeypot (ADR 0008 決定 4)。**人間は触りません。**
  const [website, setWebsite] = useState('');
  const [phase, setPhase] = useState<Phase>({ kind: 'editing' });
  // 控えの届き先 (ADR 0008 決定 2)。**未ログインなら null。**
  //
  // **上の `email` とは別に持ちます。** あちらは入力欄で、利用者が
  // 書き換えられます —— 書き換えられた値を「控えはここへ届きます」と
  // 表示すると、**嘘の案内**になります。控えが届くのは常に
  // アカウントに登録されているアドレスのほうです。
  const [accountEmail, setAccountEmail] = useState<string | null>(null);

  // ログイン済みなら氏名とアドレスを埋めます (決定 4)。
  //
  // **未ログインは失敗ではありません。** 匿名でも問い合わせできる ——
  // むしろ「ログインできない」という問い合わせが来る前提なので、
  // 401 は黙って無視します。
  useEffect(() => {
    let alive = true;
    // ヘッダのナビも同じものを引くので、`loadMe` を通して 1 回で済ませます。
    loadMe()
      .then((me) => {
        if (!alive || me === null) return;
        // **入力済みの値は上書きしません。** 取得は非同期なので、
        // 先に打ち始めていた文字を消してしまいます。
        setName((v) => (v === '' ? me.displayName : v));
        setEmail((v) => (v === '' ? me.email : v));
        // **こちらは入力済みでも上書きします。** 入力欄と違って
        // 利用者が触る値ではなく、「控えがどこへ届くか」という事実です。
        setAccountEmail(me.email);
      })
      .catch(() => undefined);
    return () => {
      alive = false;
    };
  }, []);

  const filled = name.trim() !== '' && email.trim() !== '' && subject.trim() !== '' && body.trim() !== '';

  async function onSubmit() {
    setPhase({ kind: 'sending' });
    try {
      await submitContact({ name, email, subject, body, website });
      setPhase({ kind: 'accepted' });
      setName('');
      setEmail('');
      setSubject('');
      setBody('');
    } catch (e) {
      setPhase({ kind: 'error', message: describeContactError(e) });
    }
  }

  if (phase.kind === 'accepted') {
    return (
      <div className="card">
        <p role="status">問い合わせを受け付けました。</p>
        {/*
          **「送信しました」と書かない。** 受理までしか終わっていません
          (ADR 0008 決定 1)。ここを正確に書くことが、
          202 を返すことにした理由そのものになります。
        */}
        {/*
          **入力欄の値は使わない。** ここに表示するのは accountEmail で、
          利用者が書き換えられる email 欄ではありません (ADR 0008 決定 2)。
        */}
        {accountEmail === null ? (
          <p className="muted measure">
            運営への通知は順に送られます。<strong>控えのメールは届きません</strong> ——
            入力されたアドレスは検証していないため、そこへメールを送らない設計です
            (返信は担当者が手で行います)。
          </p>
        ) : (
          <p className="muted measure">
            運営への通知は順に送られます。控えを <strong>{accountEmail}</strong> 宛に
            お送りします —— アカウントに登録されているアドレスで、
            入力されたアドレスへは送りません。
          </p>
        )}
        <div className="actions">
          <button type="button" className="btn" onClick={() => setPhase({ kind: 'editing' })}>
            もう 1 件送る
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="card">
      <div className="field">
        <label className="field__label" htmlFor="contact-name">
          お名前
        </label>
        <input
          id="contact-name"
          className="input"
          value={name}
          maxLength={limits.name}
          onChange={(e) => setName(e.target.value)}
        />
      </div>

      <div className="field">
        <label className="field__label" htmlFor="contact-email">
          メールアドレス
        </label>
        <input
          id="contact-email"
          type="email"
          className="input"
          value={email}
          maxLength={limits.email}
          onChange={(e) => setEmail(e.target.value)}
        />
        <span className="field__hint">
          {accountEmail === null
            ? 'このアドレスへメールは送りません (検証していないアドレスへ送ると、このシステムが踏み台になるためです)。担当者からの返信先として記録します。'
            : `このアドレスへメールは送りません。控えは登録アドレス (${accountEmail}) 宛にお送りします。`}
        </span>
      </div>

      <div className="field">
        <label className="field__label" htmlFor="contact-subject">
          件名
        </label>
        <input
          id="contact-subject"
          className="input"
          value={subject}
          maxLength={limits.subject}
          onChange={(e) => setSubject(e.target.value)}
        />
      </div>

      <div className="field">
        <label className="field__label" htmlFor="contact-body">
          お問い合わせ内容
        </label>
        <textarea
          id="contact-body"
          className="textarea"
          value={body}
          maxLength={limits.body}
          onChange={(e) => setBody(e.target.value)}
        />
        <span className="field__hint">
          {body.length} / {limits.body} 文字
        </span>
      </div>

      {/*
        honeypot (ADR 0008 決定 4)。**画面には出しません。**

        `display: none` で消したうえで、次の 3 つを付けています。
          tabIndex={-1}      Tab キーで到達しない (キーボード操作でも埋まらない)
          autoComplete="off" **ブラウザの自動入力に埋められないため**
          aria-hidden        スクリーンリーダーに読ませない

        2 つ目が要点です。埋まっていると API 側で**黙って破棄される**ので、
        自動入力に埋められると人間の問い合わせが消えます。
        honeypot を選ぶ以上ここは 0 にはできませんが、確率は下げられます。
      */}
      <div style={{ display: 'none' }} aria-hidden="true">
        <label htmlFor="contact-website">この欄は入力しないでください</label>
        <input
          id="contact-website"
          name="website"
          tabIndex={-1}
          autoComplete="off"
          value={website}
          onChange={(e) => setWebsite(e.target.value)}
        />
      </div>

      <div className="actions">
        <button
          type="button"
          className="btn btn--primary"
          disabled={!filled || phase.kind === 'sending'}
          onClick={() => void onSubmit()}
        >
          {phase.kind === 'sending' ? '送信中…' : '送信する'}
        </button>
      </div>

      {phase.kind === 'error' && (
        <p className="alert alert--error" role="alert">
          {phase.message}
        </p>
      )}
    </div>
  );
}

/**
 * エラーを利用者向けの文言にします。
 *
 * **`code` で分岐します** —— `message` は人間向けで予告なく変わります
 * (仕様書の `Error` スキーマ)。
 */
function describeContactError(e: unknown): string {
  if (!(e instanceof ApiError)) {
    return '送信できませんでした。通信環境を確かめて、もう一度お試しください。';
  }
  switch (e.code) {
    // 429。**何件までなら通るかは API も返しません** (攻撃側にだけ有用なため)。
    // 画面でも上限を書かず、「時間をおく」とだけ伝えます。
    case 'RESOURCE_EXHAUSTED':
      return '短時間に送りすぎています。しばらく時間をおいてからお試しください。';
    case 'INVALID_ARGUMENT':
      return `入力を確認してください: ${e.message}`;
    // 403 は CSRF の検査 (ADR 0013 決定 1)。利用者の入力の問題ではないので、
    // 「入力を確認」とは書きません。
    case 'PERMISSION_DENIED':
      return '送信が拒否されました。ページを開き直してからお試しください。';
    default:
      return '送信できませんでした。時間をおいてもう一度お試しください。';
  }
}
