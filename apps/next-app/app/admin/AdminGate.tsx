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
import { useEffect, useState, type ReactNode } from 'react';

import { ApiError, getMe, loginUrl, type Me, type Role } from '../lib/api';
import { card, colors, heading, page } from '../lib/ui';

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
  const [state, setState] = useState<State>({ kind: 'loading' });

  useEffect(() => {
    // **画面を離れたあとに setState しない。**
    // 開発中の Strict Mode では effect が 2 回走るので、
    // 片付けないと解決済みの古い応答で状態が上書きされます。
    let alive = true;

    getMe()
      .then((me) => {
        if (!alive) return;
        setState(
          rank[me.role] >= rank[require] ? { kind: 'ready', me } : { kind: 'forbidden', me },
        );
      })
      .catch((e: unknown) => {
        if (!alive) return;
        // **401 だけを「未ログイン」に倒します。**
        // それ以外 (API が落ちている等) を未ログイン扱いにすると、
        // 障害中にログイン画面へ送り続けることになります。
        if (e instanceof ApiError && e.code === 'UNAUTHENTICATED') {
          setState({ kind: 'anonymous' });
          return;
        }
        setState({ kind: 'error', message: describe(e) });
      });

    return () => {
      alive = false;
    };
  }, [require]);

  return (
    <div style={page}>
      <h1 style={heading}>{title}</h1>
      {state.kind === 'loading' && <p style={{ color: colors.dim }}>確認しています...</p>}

      {state.kind === 'anonymous' && (
        <div style={card}>
          <p>ログインが必要です。</p>
          {/*
            **Next.js の <Link> ではありません。** 遷移先は Go API の
            302 で、クライアント側ルーティングでは辿れません。
          */}
          <a href={loginUrl()} style={{ color: colors.fg }}>
            Google でログインする
          </a>
        </div>
      )}

      {state.kind === 'forbidden' && (
        <div style={card}>
          <p style={{ color: colors.danger }}>この画面を開く権限がありません。</p>
          <p style={{ color: colors.dim }}>
            必要なロール: {require} / 現在のロール: {state.me.role}
          </p>
          {/*
            **ログイン導線は出しません。** 誰かは分かっている状態なので、
            ログインし直しても解決しません (ADR 0013 決定 3)。
          */}
        </div>
      )}

      {state.kind === 'error' && (
        <div style={card}>
          <p style={{ color: colors.danger }}>状態を確認できませんでした。</p>
          <p style={{ color: colors.dim }}>{state.message}</p>
        </div>
      )}

      {state.kind === 'ready' && (
        <>
          <AdminNav role={state.me.role} />
          {children(state.me)}
        </>
      )}
    </div>
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
    <nav style={{ margin: '0 0 1.5rem', color: colors.dim }}>
      <Link href="/admin" style={{ color: colors.fg, marginRight: '1rem' }}>
        通報キュー
      </Link>
      {rank[role] >= rank.admin && (
        <Link href="/admin/roles" style={{ color: colors.fg }}>
          ロールの変更
        </Link>
      )}
    </nav>
  );
}

/**
 * 例外を画面に出せる 1 行にします。
 *
 * **`ApiError` は `code` を添えます。** 文言は予告なく変わるので、
 * 問い合わせのときに手がかりになるのはコードのほうです。
 */
export function describe(e: unknown): string {
  if (e instanceof ApiError) {
    return `${e.code}: ${e.message}`;
  }
  if (e instanceof Error) {
    return e.message;
  }
  return String(e);
}
