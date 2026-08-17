'use client';

// 管理画面の入口。**ロールを見て中身を出し分けます。**
//
// 【これは防御ではありません】
// 権限の判定は**サーバが行います** —— この画面を出さないことと、
// API が拒否することは別物です。ここで隠しても、API を直接叩けば通ります。
// 逆に、ここで通しても API が 403 を返せば何もできません。
//
// つまりこの部品の役目は「押せないボタンを見せない」ことだけです。
// **API 側の検査を省く理由にはなりません** (moderation/usecase が持っています)。
//
// 【401 と 403 を出し分ける】
// ADR 0013 決定 3 が名指しした間違えやすい箇所です。
//
//   - 401 —— 誰か分からない。**ログインすれば解決する**
//   - 403 —— 誰かは分かるが権限がない。**ログインしても解決しない**
//
// 取り違えると、権限のない利用者がログイン画面に飛ばされ続けます。
import Link from 'next/link';
import { useMemo, type ReactNode } from 'react';

import { loginUrl, type Me, type Role } from '../lib/api';
import { useMe } from '../lib/me';

/** ロールの強さ。**数値の大小で比較します** (仕様書の Role と同じ 3 値)。 */
const rank: Record<Role, number> = { user: 0, moderator: 1, admin: 2 };

type State =
  | { kind: 'loading' }
  | { kind: 'ready'; me: Me }
  | { kind: 'anonymous' }
  | { kind: 'forbidden'; me: Me }
  | { kind: 'error'; message: string };

export function AdminGate({
  title,
  require,
  children,
}: {
  title: string;
  /** この画面に必要な最低ロール。 */
  require: Role;
  children: (me: Me) => ReactNode;
}) {
  // ログイン状態そのものは `useMe` が持ちます (ヘッダのナビと共有するため)。
  // **ここが決めるのは「その状態でこの画面を開けるか」だけ**になります。
  const { state: meState } = useMe();

  // **効果で状態を作り直しません。** ログイン状態から一意に決まるので、
  // 複製すると 2 つの真実ができ、片方だけ古い瞬間が生まれます。
  const state: State = useMemo(() => {
    switch (meState.kind) {
      case 'loading':
        return { kind: 'loading' };
      // **401 だけが「未ログイン」です。** それ以外 (API が落ちている等) を
      // 未ログイン扱いにすると、障害中にログイン画面へ送り続けることになります。
      case 'anonymous':
        return { kind: 'anonymous' };
      case 'error':
        return { kind: 'error', message: meState.message };
      case 'ready':
        return rank[meState.me.role] >= rank[require]
          ? { kind: 'ready', me: meState.me }
          : { kind: 'forbidden', me: meState.me };
    }
  }, [meState, require]);

  return (
    <>
      <h1>{title}</h1>
      {state.kind === 'loading' && (
        <p className="muted" aria-live="polite">
          確認しています...
        </p>
      )}

      {state.kind === 'anonymous' && (
        <div className="card">
          <p>ログインが必要です。</p>
          {/*
            **Next.js の <Link> ではありません。** 遷移先は Go API の
            302 で、クライアント側ルーティングでは辿れません。
          */}
          <a className="btn" href={loginUrl()}>
            Google でログインする
          </a>
        </div>
      )}

      {state.kind === 'forbidden' && (
        <div className="alert alert--error" role="alert">
          <p>この画面を開く権限がありません。</p>
          <p className="muted">
            必要なロール: {require} / 現在のロール: {state.me.role}
          </p>
          {/*
            **ログイン導線は出しません。** 誰かは分かっている状態なので、
            ログインし直しても解決しません (ADR 0013 決定 3)。
          */}
        </div>
      )}

      {state.kind === 'error' && (
        <div className="alert alert--error" role="alert">
          <p>状態を確認できませんでした。</p>
          <p className="muted">{state.message}</p>
        </div>
      )}

      {state.kind === 'ready' && (
        <>
          <AdminNav role={state.me.role} />
          {children(state.me)}
        </>
      )}
    </>
  );
}

/**
 * 管理画面どうしの行き来。
 *
 * **開けない画面へのリンクは出しません。** モデレーターに
 * 「ロールの変更」を見せても 403 になるだけで、
 * ロールの意味を誤解させます (決定 1 は権限を分離するためのものです)。
 */
function AdminNav({ role }: { role: Role }) {
  return (
    <nav className="site-nav" aria-label="管理画面の切り替え">
      <Link href="/admin">通報キュー</Link>
      {rank[role] >= rank.admin && <Link href="/admin/roles">ロールの変更</Link>}
    </nav>
  );
}
