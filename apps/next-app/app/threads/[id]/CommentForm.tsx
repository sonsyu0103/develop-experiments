'use client';

// コメントの投稿。
//
// 【二重投稿を防ぐのはボタンの無効化ではありません】
// 送信中にボタンを無効にしても、**「タイムアウトしたが実は成功していた」**
// は防げません (ADR 0015 決定 1)。画面には失敗と出て、サーバには 1 件入る。
// 利用者はもう一度押し、2 件になります。
//
// 防いでいるのは `Idempotency-Key` です。同じキーで送り直すと、
// サーバは処理をやり直さず前回の結果を返します。
//
// 【キーは「同じ内容のあいだ」だけ使い回します】
// 内容を変えたのに同じキーで送ると **422** になります ——
// 黙って前回の結果を返すと、クライアントのバグが見えなくなるためです。
// そこで、入力内容の指紋が変わったらキーを作り直します。
// 「失敗 → そのまま再送」は同じキー、「失敗 → 書き直して再送」は新しいキー、
// という**利用者の操作どおりの意味**になります。
//
// 【未ログインではキーが無視されます】
// 匿名にはキーの名前空間を分ける手段が無く、IP で分けると NAT の背後で
// 他人と衝突します (同 決定 4)。この非対称は仕様として明記されています ——
// **匿名投稿では二重投稿が起こりうる**、が正しい理解になります。
import { useId, useRef, useState } from 'react';

import { ApiError, createComment, type Comment, type Image } from '../../lib/api';
import { describeWriteError } from '../../lib/errors';
import { ImageField } from '../../components/ImageField';
import { cx } from '../../lib/ui';

/** **仕様書の maxLength と同じ値です** (CreateCommentRequest)。 */
const limits = { body: 2000, authorName: 50 } as const;

type Phase =
  | { kind: 'editing' }
  | { kind: 'sending' }
  | { kind: 'error'; message: string };

/**
 * 冪等キーを作ります。
 *
 * **`crypto.randomUUID` は安全なコンテキストでしか使えません** ——
 * https / localhost 以外 (LAN の IP で開いた開発機など) では
 * `undefined` になります。そこで落とすと、そういう環境では
 * 投稿そのものができなくなるので、推測されにくい値に落とします。
 * キーの役目は再送の同一性の判定で、秘密ではありません。
 */
function newKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `k-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
}

export function CommentForm({
  threadId,
  signedIn,
  onPosted,
  unknownIdentity = false,
}: {
  threadId: number;
  signedIn: boolean;
  onPosted: (comment: Comment) => void;
  /**
   * ログイン状態が**分からない**とき (GET /me が 401 以外で失敗) に立てます。
   *
   * 匿名向けの案内 (名前欄と「あとから削除できません」) を伏せるためのものです。
   * ログイン中なのに名前欄を出すと、書いた名前はサーバが捨てて
   * **実名の表示名で公開される** —— 利用者から見れば名前を無視された形になります。
   * 画像の添付だけは、権限が分からないので出しません。
   */
  unknownIdentity?: boolean;
}) {
  const bodyId = useId();
  const nameId = useId();
  const [body, setBody] = useState('');
  const [authorName, setAuthorName] = useState('');
  const [image, setImage] = useState<Image | null>(null);
  const [phase, setPhase] = useState<Phase>({ kind: 'editing' });

  // いま送ろうとしている内容と、それに割り当てたキー。
  const attempt = useRef<{ fingerprint: string; key: string } | null>(null);

  function keyFor(fingerprint: string): string {
    if (attempt.current === null || attempt.current.fingerprint !== fingerprint) {
      attempt.current = { fingerprint, key: newKey() };
    }
    return attempt.current.key;
  }

  async function onSubmit() {
    const trimmed = body.trim();
    if (trimmed === '') return;

    const request = {
      body: trimmed,
      // **ログイン中は送りません。** 送っても無視されますが (エラーにはならない)、
      // 送らないほうが「表示名は users 側から解決される」ことが読めます (ADR 0014)。
      ...(!signedIn && authorName.trim() !== '' ? { authorName: authorName.trim() } : {}),
      ...(image === null ? {} : { imageId: image.id }),
    };

    setPhase({ kind: 'sending' });
    try {
      const posted = await createComment(threadId, request, keyFor(JSON.stringify(request)));
      // **キーを手放します。** 次の投稿は別の投稿なので、
      // 同じキーで送ると「前回の結果」が返ってしまいます。
      attempt.current = null;
      setBody('');
      setImage(null);
      setPhase({ kind: 'editing' });
      onPosted(posted);
    } catch (e) {
      setPhase({ kind: 'error', message: describePostError(e, signedIn) });
      // **422 のときだけキーを捨てます。** 前回と内容が食い違ったという
      // 申告なので、同じキーのままでは何度送っても 422 のままになります。
      if (e instanceof ApiError && e.code === 'FAILED_PRECONDITION') {
        attempt.current = null;
      }
    }
  }

  const sending = phase.kind === 'sending';
  const remaining = limits.body - body.length;

  return (
    <div className="card">
      <div className="field">
        <label className="field__label" htmlFor={bodyId}>
          コメント
        </label>
        <textarea
          id={bodyId}
          className="textarea"
          value={body}
          maxLength={limits.body}
          disabled={sending}
          placeholder="ふぁ〜、眠いよ〜"
          onChange={(e) => setBody(e.target.value)}
        />
        <span
          className={cx(
            'counter',
            remaining <= 0 && 'counter--over',
            remaining > 0 && remaining <= 100 && 'counter--near',
          )}
        >
          残り {remaining} 文字
        </span>
      </div>

      {!signedIn && !unknownIdentity && (
        <div className="field">
          <label className="field__label" htmlFor={nameId}>
            名前 (任意)
          </label>
          <input
            id={nameId}
            className="input"
            value={authorName}
            maxLength={limits.authorName}
            disabled={sending}
            placeholder="名無しさん"
            onChange={(e) => setAuthorName(e.target.value)}
          />
          <span className="field__hint">
            未入力なら「名無しさん」になります。
            {/*
              **ここを正確に書きます。** 匿名投稿は削除できません ——
              author_id が NULL で、本人であることを示せないためです
              (ADR 0005 決定 2)。あとから「消したい」と言われても手段がありません。
            */}
            匿名の投稿は、あとから自分で削除できません。
          </span>
        </div>
      )}

      <ImageField
        kind="comment_attachment"
        label="画像を添付 (任意)"
        hint="JPEG / PNG / WebP、5 MiB まで。保存時に再エンコードされます (位置情報は残りません)。"
        value={image}
        onChange={setImage}
        canUpload={signedIn && !unknownIdentity}
        disabled={sending}
      />

      <div className="actions">
        <button
          type="button"
          className="btn btn--primary"
          disabled={sending || body.trim() === ''}
          onClick={() => void onSubmit()}
        >
          {sending ? '投稿しています...' : '投稿する'}
        </button>
        {signedIn && !unknownIdentity && (
          <span className="muted">ログイン中の表示名で投稿されます。</span>
        )}
        {unknownIdentity && (
          <span className="muted">
            ログイン中ならその表示名で、そうでなければ「名無しさん」で投稿されます。
          </span>
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
 * 投稿の失敗を利用者向けの文言にします。
 *
 * **409 を「失敗」と書き切らないのが要点です。** ここは同時実行の競合
 * (レス番号の直列化失敗、または同じキーの処理が進行中) で、
 * **再試行できる**種類の失敗になります (ADR 0019)。
 * 「時間をおいて」と書くと、押せば通るものを諦めさせます。
 *
 * **ログインの有無で文言を変えます** (レビュー指摘)。
 * 冪等キーは**未ログインでは無視される**ので (ADR 0015 決定 4)、
 * 匿名の利用者に「二重には投稿されません」と案内すると嘘になります ——
 * 「タイムアウトしたが実は成功していた」場合、押し直すと本当に 2 件目ができます。
 */
function describePostError(e: unknown, signedIn: boolean): string {
  if (!(e instanceof ApiError)) {
    return '投稿できませんでした。通信環境を確かめて、もう一度お試しください。';
  }
  switch (e.code) {
    case 'CONFLICT':
      return signedIn
        ? '同時に投稿が重なりました。もう一度「投稿する」を押してください (同じ内容として扱われるので、二重には投稿されません)。'
        : '同時に投稿が重なりました。もう一度「投稿する」を押してください。ただし未ログインの投稿は二重送信を防ぐ仕組みが働かないので、押し直す前に一覧を確認してください (先ほどの投稿が入っていることがあります)。';
    case 'FAILED_PRECONDITION':
      return '前回の送信と内容が食い違いました。もう一度「投稿する」を押してください。';
    case 'NOT_FOUND':
      return 'このスレッドは見つかりません。削除された可能性があります。';
    default:
      return describeWriteError(e);
  }
}
