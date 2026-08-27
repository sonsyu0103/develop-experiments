'use client';

// 画像の削除。
//
// 【なぜ独立した画面が要るか】
// **投稿を消しても画像は残ります。** 回収バッチが拾うのは
// 「添付されていない画像」と「`status = 'deleted'` の画像」だけで、
// コメントに添付済みの画像は、そのコメントが論理削除されても
// どちらにも当たりません —— S3 に残り続け、**URL を直接叩けば見えます。**
// ADR 0011 決定 5 が消そうとしていたのはこの状態です。
//
// 通報は画像を対象にできない (`ReportTargetType` は thread / comment のみ) ため、
// キューからは辿れません。ここが `delete_image` の唯一の導線になります。
//
// 【消えるまでには 2 段の遅れがある】
//   - S3 から消えるまで —— 回収バッチの周回間隔 (既定 10 分)
//   - CDN から消えるまで —— **未実装。** キャッシュ期間ぶん残ります
//     (ADR 0011 の「実装して分かったこと 11」)
import { useState } from 'react';

import { ApiError, createModerationAction } from '../lib/api';
import { describe } from '../lib/errors';

// 画像の URL は `.../{uuid}.jpg` の形なので、貼り付けた URL から拾えます。
// **UUID をそのまま入れてもよい**ようにしています ——
// モデレーターが持っているのは大抵 URL のほうです。
const uuidPattern = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i;

export function ImageDeleteForm() {
  const [raw, setRaw] = useState('');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<string | null>(null);

  const found = uuidPattern.exec(raw);
  // **小文字に正規化します。** API は正規化した UUID で記録するので、
  // 入力の大文字小文字で記録が揺れないようにここでも揃えます。
  const imageID = found === null ? null : found[0].toLowerCase();

  async function onSubmit() {
    if (imageID === null) return;

    setBusy(true);
    setResult(null);
    try {
      const recorded = await createModerationAction({
        action: 'delete_image',
        targetId: imageID,
        ...(reason.trim() ? { reason: reason.trim() } : {}),
      });
      setResult(
        `削除しました (記録 #${recorded.id} / 対象 ${recorded.targetId})。` +
          'S3 からは回収バッチが消します (既定 10 分間隔)。CDN のキャッシュは残ります。',
      );
      setRaw('');
      setReason('');
    } catch (e) {
      setResult(
        e instanceof ApiError && e.code === 'NOT_FOUND'
          ? '見つからないか、既に削除・回収済みです'
          : describe(e),
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card">
      <div className="field">
        <label className="field__label" htmlFor="image-id">
          画像の URL または ID
        </label>
        <input
          id="image-id"
          className="input"
          value={raw}
          placeholder="https://.../018f2c00-0000-7000-8000-000000000001.jpg"
          onChange={(e) => setRaw(e.target.value)}
        />
        {raw !== '' && imageID === null && (
          <p className="alert alert--error" role="alert">
            UUID を読み取れません。画像の URL をそのまま貼っても構いません。
          </p>
        )}
        {imageID !== null && <span className="field__hint">対象: {imageID}</span>}
      </div>

      <div className="field">
        <label className="field__label" htmlFor="image-reason">
          削除の理由 (任意・監査記録に残る)
        </label>
        <input
          id="image-reason"
          className="input"
          value={reason}
          maxLength={500}
          onChange={(e) => setReason(e.target.value)}
        />
      </div>

      <div className="actions">
        <button
          type="button"
          className="btn btn--danger"
          disabled={busy || imageID === null}
          onClick={() => void onSubmit()}
        >
          画像を削除
        </button>
      </div>

      {result !== null && (
        <p className="alert alert--warn" role="status">
          {result}
        </p>
      )}
    </div>
  );
}
