'use client';

// スレッドの作成。
//
// 【冪等キーはありません】
// コメント投稿と違い、この経路は `Idempotency-Key` を受け付けません
// (仕様書)。二重に立ってしまった場合は、自分のスレッドなら削除できます ——
// **匿名で立てた場合は消せません。** そこは画面で先に伝えます。
//
// 【アイコンは任意で、ログインが要ります】
// 画像の投稿にログインが要るため (ADR 0007)、未ログインでは
// アイコンなしのスレッドだけになります。**未ログインで立てられないのは
// アイコンだけ**で、スレッド自体は立てられます。
import { useId, useState } from 'react';
import { useRouter } from 'next/navigation';

import { ApiError, createThread, loginUrl, type Image } from '../../lib/api';
import { describeWriteError } from '../../lib/errors';
import { useMe } from '../../lib/me';
import { ImageField } from '../../components/ImageField';
import { cx } from '../../lib/ui';

/** **仕様書と DB の CHECK 制約 `threads_title_length` と同じ値です。** */
const titleLimit = 200;

type Phase = { kind: 'editing' } | { kind: 'sending' } | { kind: 'error'; message: string };

export function NewThreadForm() {
  const router = useRouter();
  const titleId = useId();
  const { state: meState } = useMe();
  const signedIn = meState.kind === 'ready';
  // **決まるまでは「できない」と言い切らない** (レビュー指摘)。
  // GET /me の往復中は必ず signedIn === false なので、ログイン中の利用者にも
  // 「画像を使うにはログインが必要です」とログインボタンが出て、
  // 応答が返った瞬間に入れ替わります —— ThreadView が避けている
  // 「押そうとした先が変わる」挙動が、この画面には残っていました。
  //
  // `error` も同じ扱いにします。401 以外を未ログイン扱いにしないのが
  // me.ts の設計なので (ADR 0013 決定 3)、error は「分からない」になります。
  const meResolved = meState.kind === 'ready' || meState.kind === 'anonymous';

  const [title, setTitle] = useState('');
  const [icon, setIcon] = useState<Image | null>(null);
  const [phase, setPhase] = useState<Phase>({ kind: 'editing' });

  async function onSubmit() {
    const trimmed = title.trim();
    if (trimmed === '') return;

    setPhase({ kind: 'sending' });
    try {
      const thread = await createThread({
        title: trimmed,
        ...(icon === null ? {} : { iconImageId: icon.id }),
      });
      // **`sending` のままにします。** ここで `editing` に戻すと、
      // 遷移が終わるまでのあいだボタンが押せる状態に戻り、
      // 2 本目が立ちます (この経路には冪等キーがありません)。
      router.push(`/threads/${thread.id}`);
      // 一覧は Server Component なので、控えを捨てないと
      // 戻ったときに新しいスレッドが出ません。
      router.refresh();
    } catch (e) {
      setPhase({ kind: 'error', message: describeCreateError(e) });
    }
  }

  const sending = phase.kind === 'sending';
  const remaining = titleLimit - title.length;

  return (
    <div className="card">
      <div className="field">
        <label className="field__label" htmlFor={titleId}>
          タイトル
        </label>
        <input
          id={titleId}
          className="input"
          value={title}
          maxLength={titleLimit}
          disabled={sending}
          placeholder="Go の並列処理を学ぶ部屋"
          onChange={(e) => setTitle(e.target.value)}
        />
        <span className={cx('counter', remaining <= 20 && 'counter--near')}>
          残り {remaining} 文字
        </span>
      </div>

      <ImageField
        kind="thread_icon"
        label="アイコン (任意)"
        hint="小さい正方形として保存されます。JPEG / PNG / WebP、5 MiB まで。"
        value={icon}
        onChange={setIcon}
        canUpload={signedIn}
        // 決まるまでは案内を出さない (上の meResolved のコメント)。
        uploadHintSuppressed={!meResolved}
        disabled={sending}
      />

      <div className="actions">
        <button
          type="button"
          className="btn btn--primary"
          disabled={sending || title.trim() === ''}
          onClick={() => void onSubmit()}
        >
          {sending ? '作成しています...' : 'スレッドを立てる'}
        </button>
        {meResolved && !signedIn && (
          <a href={loginUrl()} className="btn btn--quiet">
            Google でログインする
          </a>
        )}
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
 * 作成の失敗を利用者向けの文言にします。
 *
 * **404 を「見つかりません」と出しません。** この経路の 404 は
 * 「指定したアイコンが存在しないか、自分のものではない」で、
 * スレッドの話ではありません (403 にすると画像の存在が漏れるため
 * 404 になっています)。そのまま出すと、何が無いのか伝わりません。
 */
function describeCreateError(e: unknown): string {
  if (e instanceof ApiError) {
    switch (e.code) {
      case 'NOT_FOUND':
        return 'アイコンに指定した画像が見つかりません。選び直してからお試しください。';
      case 'UNAUTHENTICATED':
        return 'アイコンを付けるにはログインが必要です。アイコンを外せば、そのまま立てられます。';
      case 'UNAVAILABLE':
        return 'いま画像の保管庫が使えません。アイコンを外せば、そのまま立てられます。';
      default:
        break;
    }
  }
  return describeWriteError(e);
}
