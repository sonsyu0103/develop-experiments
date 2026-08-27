'use client';

// プロフィールの表示と、プロフィール画像の変更。
//
// 【自分の投稿一覧は MyPosts が持ちます】
// 長らく「投稿者で絞る API が無い」と断りを出していた場所です。
// `GET /me/threads` と `GET /me/comments` を入れたので、実物に置き換えました。
// **懸案だった「コメントを投稿者で絞ると 8 区画すべてを走る」は測って通しました**
// —— 実測 1.5 ms で、索引の追加は不要という結論です
// (db/query/comments.sql に数字が残してあります)。
//
// 【表示名は変えられません】
// Google から来た名前をそのまま使います (ADR 0005)。
// 変更を受け付けるには本人確認とは別に、表示名の重複・改名の履歴を
// どう扱うかを決める必要があります。
import { useState } from 'react';
import { useRouter } from 'next/navigation';

import { loginUrl, logout, setMyAvatar, type Image, type Me } from '../lib/api';
import { describeWriteError } from '../lib/errors';
import { invalidateMe, primeMe, useMe } from '../lib/me';
import { ImageField } from '../components/ImageField';

import { MyPosts } from './MyPosts';

/** ロールの説明。**画面に出す言葉で権限の境界を伝えます** (ADR 0011 決定 1)。 */
const roleNote: Record<Me['role'], string> = {
  user: '自分の投稿だけを削除できます。',
  moderator: '他人・匿名の投稿も削除できます。ロールの変更はできません。',
  admin: '投稿の削除に加えて、利用者のロールを変更できます。',
};

export function MyProfile() {
  const router = useRouter();
  const { state, reload } = useMe();
  const [saving, setSaving] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  // **ログアウトの失敗は別に持ちます** (レビュー指摘)。
  // アバターと同じ state に入れると、**画面下の「プロフィール画像」カードの中**に
  // ログアウトの失敗が出ます。ボタンの周りには成功も失敗も出ないので、
  // 共有端末で「押したから切れた」と読まれる余地が残ります。
  const [logoutError, setLogoutError] = useState<string | null>(null);

  /**
   * アバターを差し替えます。
   *
   * **アップロードと設定は 2 段です。** `POST /images` で上げた画像は
   * まだどこからも参照されていないので、`PUT /me/avatar` で結び付けます。
   */
  async function applyAvatar(image: Image | null) {
    setSaving(true);
    setError(null);
    setNotice(null);
    try {
      const me = await setMyAvatar(image === null ? null : image.id);
      // **応答が新しい `Me` を返すので、もう一往復させません。**
      // `primeMe` は購読している部品すべてに伝わるので、
      // この画面だけでなくヘッダの表示も同時に変わります。
      primeMe(me);
      setNotice(
        image === null
          ? 'プロフィール画像を外しました (Google のプロフィール画像に戻ります)。'
          : 'プロフィール画像を変更しました。',
      );
    } catch (e) {
      setError(describeWriteError(e));
    } finally {
      setSaving(false);
    }
  }

  async function onLogout() {
    setSaving(true);
    setLogoutError(null);
    try {
      await logout();
      // **控えを必ず捨てます。** Cookie は消えているのに画面だけ
      // ログイン中のまま、という状態がいちばん分かりにくくなります。
      // ヘッダの「管理」リンクもここで消えます (購読しているため)。
      invalidateMe();
      setNotice('ログアウトしました。');
      // Server Component 側の出力も引き直します。
      router.refresh();
    } catch (e) {
      setLogoutError(describeWriteError(e));
    } finally {
      setSaving(false);
    }
  }

  if (state.kind === 'loading') {
    return (
      <p className="muted" aria-live="polite">
        ログイン状態を確認しています...
      </p>
    );
  }

  if (state.kind === 'error') {
    return (
      <div className="alert alert--error" role="alert">
        <p>状態を確認できませんでした。</p>
        <p className="muted">{state.message}</p>
        <div className="actions">
          <button type="button" className="btn" onClick={reload}>
            再試行する
          </button>
        </div>
      </div>
    );
  }

  if (state.kind === 'anonymous') {
    return (
      <div className="card">
        <p>ログインしていません。</p>
        <p className="muted measure">
          ログインすると、画像の添付・自分の投稿の削除・通報ができるようになります。
          スレッドを立てることとコメントの投稿は、ログインしなくてもできます。
        </p>
        {/*
          **Next.js の <Link> ではありません。** 遷移先は Go API の 302 で、
          クライアント側ルーティングでは辿れません。
        */}
        <a className="btn btn--primary" href={loginUrl()}>
          Google でログインする
        </a>
        {notice !== null && (
          <p className="alert alert--ok" role="status">
            {notice}
          </p>
        )}
      </div>
    );
  }

  const me = state.me;

  return (
    <>
      <div className="card">
        <div className="thread">
          {me.avatarUrl !== undefined && me.avatarUrl !== null && (
            // eslint-disable-next-line @next/next/no-img-element
            <img className="avatar avatar--lg" src={me.avatarUrl} width={72} height={72} alt="" />
          )}
          <div>
            <h2>{me.displayName}</h2>
            <p className="meta">
              <span>{me.email}</span>
              <span className="badge">{me.role}</span>
            </p>
            <p className="muted">{roleNote[me.role]}</p>
          </div>
        </div>

        <p className="field__hint">
          {/*
            **内部 ID (users.id) は API が返しません。** マイページの URL が
            連番だと全利用者を列挙できるため、外に出る識別子は公開 ID だけです
            (ADR 0003 未決 #11)。ロールの変更もこの ID で行います。
          */}
          公開 ID: <code>{me.publicId}</code>
        </p>

        <div className="actions">
          <button type="button" className="btn" disabled={saving} onClick={() => void onLogout()}>
            ログアウト
          </button>
        </div>
        {logoutError !== null && (
          <p className="alert alert--error" role="alert">
            {logoutError}
          </p>
        )}
        <p className="field__hint">
          セッションの実体はサーバ側にあるので、ログアウトは即座に効きます。
        </p>
      </div>

      <h2>プロフィール画像</h2>
      <div className="card">
        <ImageField
          kind="avatar"
          label="新しい画像を選ぶ"
          hint="正方形に近い形で保存されます。設定を外すと Google のプロフィール画像に戻ります。"
          value={null}
          onChange={(image) => {
            if (image !== null) void applyAvatar(image);
          }}
          canUpload
          disabled={saving}
        />

        <div className="actions">
          <button
            type="button"
            className="btn btn--quiet"
            disabled={saving}
            onClick={() => void applyAvatar(null)}
          >
            設定を外す
          </button>
        </div>

        {notice !== null && (
          <p className="alert alert--ok" role="status">
            {notice}
          </p>
        )}
        {error !== null && (
          <p className="alert alert--error" role="alert">
            {error}
          </p>
        )}
      </div>

      <MyPosts />
    </>
  );
}
