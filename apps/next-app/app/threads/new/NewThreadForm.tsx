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
  const { state: meState, reload: reloadMe } = useMe();

  // **再確認の最中に、出していたものを引っ込めません** (レビュー指摘)。
  //
  // reload は同期で `loading` に戻すので、素直に書くと押した瞬間に
  // **ログイン導線・警告・再確認ボタンが 3 つとも消えます** (実測)。
  // この画面のログイン入口はここにしか無いので、応答が遅いほど
  // 「押したら何も無くなった」状態が伸びます。
  // ThreadView の formShown と同じ考え方で、直前に分かっていた状態を使います。
  //
  // **押した事実だけを持ちます。** レンダー中に ref を読む形は
  // react-hooks/refs が、効果の中の同期 setState は
  // react-hooks/set-state-in-effect が禁じています (両方で lint が落ちた)。
  // 直前の状態を持ち回らなくても、「再確認を押したあとの loading」は
  // イベントハンドラで立てた印だけで分かります。
  const [recheckRequested, setRecheckRequested] = useState(false);
  const rechecking = recheckRequested && meState.kind === 'loading';

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
  // **`error` は「確認しています」で固めない** (レビュー指摘)。
  // 待っても変わらないうえ、この画面にはログインの導線がここにしか無い ——
  // meResolved で隠すと、/me が落ちている間**ログインできなくなる**
  // (直す前は少なくともボタンは出ていたので、純粋な後退だった)。
  // 再取得の最中も「分からない」のまま扱います —— 押した瞬間に
  // ログイン導線や警告が消えないようにするため。
  const meUnknown = meState.kind === 'error' || rechecking;

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
        // 再確認の最中も、直前にログイン中だったなら使えるままにします ——
        // 1 秒の再取得のために書きかけの選択を落とす理由がありません。
        canUpload={signedIn}
        // 決まるまでは「ログインが必要」と言い切らない (上のコメント)。
        uploadBlockedBy={meUnknown ? 'unknown' : meResolved ? 'anonymous' : 'checking'}
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
        {(meResolved || meUnknown) && !signedIn && (
          <a href={loginUrl()} className="btn btn--quiet">
            Google でログインする
          </a>
        )}
      </div>

      {meUnknown && (
        <p className="alert alert--warn" role="status">
          ログイン状態を確認できませんでした。{' '}
          <button
            type="button"
            className="btn btn--quiet"
            // 再取得の最中は押せないようにします (二重に走らせない)。
            // **消しはしません** —— 消すと「押したら無くなった」になります。
            disabled={rechecking}
            onClick={() => {
              setRecheckRequested(true);
              reloadMe();
            }}
          >
            もう一度確認する
          </button>
        </p>
      )}

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
