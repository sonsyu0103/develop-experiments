'use client';

// 画像を選んでアップロードする欄。
//
// コメント添付・スレッドアイコン・プロフィール画像の 3 か所で使います。
// **用途 (`kind`) で保存形式が変わります** (ADR 0007 決定 6) が、
// 画面から見た手順は同じです。
//
// 【アップロードと添付は別の操作です】
// ここが終わっても、画像はまだ**どこからも参照されていません。**
// 得られた `id` を投稿側 (`imageId` / `iconImageId` / `PUT /me/avatar`) に
// 渡して初めて紐づきます。回収バッチは「添付されていない画像」を消すので、
// 投稿せずに離脱した画像は放っておけば消えます。
//
// 【選んだ時点で上げます】
// 投稿ボタンまで待つ形にすると、
//
//   - 送信が「アップロード + 投稿」の 2 段になり、**片方だけ失敗する**
//     状態を利用者が理解できない
//   - 冪等キーの指紋に `imageId` を含められない (投稿の直前まで ID が無い)
//
// 代わりに、画像だけ上げて投稿しなかった場合の孤児が増えます。
// これは回収バッチが消すので、実害の小さいほうを取っています。
import { useId, useRef, useState } from 'react';

import { ApiError, uploadImage, type Image, type ImageKind } from '../lib/api';

/**
 * ブラウザ側で先に弾く上限 (5 MiB)。**仕様書と同じ値です。**
 *
 * サーバ側の検査が正で、ここは往復を省くためだけのものです ——
 * 5 MiB の送信が終わってから 413 が返るのは、回線の細い環境で数十秒かかります。
 */
const maxBytes = 5 * 1024 * 1024;

/** 受け付ける形式。**SVG は含めません** —— スクリプトと外部参照を含められます。 */
const accept = 'image/jpeg,image/png,image/webp';

type Props = {
  kind: ImageKind;
  label: string;
  /** 補足。任意項目であることや、用途ごとの注意を書きます。 */
  hint?: string;
  value: Image | null;
  onChange: (image: Image | null) => void;
  /**
   * ログイン済みか。**未ログインでは画像を扱えません**
   * (匿名で任意のバイト列を置けると、容量の消費と違法コンテンツの設置が
   * 追跡不能な形で可能になるため。ADR 0007)。
   */
  canUpload: boolean;
  /**
   * `canUpload` が false のときに出す理由です。
   *
   * **「ログインが必要です」と言い切れるのは、決まったときだけ**になります。
   * 決まる前に言い切ると、ログイン中の利用者にも一瞬「使えません」と出て、
   * 応答が返った瞬間に入れ替わります (レビュー指摘)。
   * 分からないまま固まる状態と、確認中とも区別します ——
   * 前者は待っても変わらないので、そう書かないと待たせ続けることになります。
   */
  uploadBlockedBy?: 'anonymous' | 'checking' | 'unknown';
  disabled?: boolean;
};

const uploadBlockedText: Record<'anonymous' | 'checking' | 'unknown', string> = {
  anonymous: '画像を使うにはログインが必要です。',
  checking: 'ログイン状態を確認しています...',
  unknown: 'ログイン状態が分からないため、画像は使えません。',
};

export function ImageField({
  kind,
  label,
  hint,
  value,
  onChange,
  canUpload,
  uploadBlockedBy = 'anonymous',
  disabled,
}: Props) {
  const inputId = useId();
  const statusId = useId();
  const inputRef = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onPick(file: File) {
    setError(null);

    if (file.size > maxBytes) {
      setError('5 MiB を超えています。縮小してからお試しください。');
      reset();
      return;
    }

    setBusy(true);
    try {
      onChange(await uploadImage(file, kind));
    } catch (e) {
      // **既に添付済みのものは外しません** (レビュー指摘)。
      // 画像 A を添付した状態で B を選んで失敗したとき、A まで消すと
      // 下書きから黙って添付が消えます —— しかも文言は
      // 「アップロードできませんでした」としか言わないので、気づけません。
      // 5 MiB 超過の経路 (上) と挙動を揃えました。
      setError(describeUploadError(e));
    } finally {
      setBusy(false);
      // **選び直せるようにします。** 値を残すと、同じファイルを選んでも
      // `change` が発火せず、失敗したあとにやり直せません。
      reset();
    }
  }

  function reset() {
    if (inputRef.current !== null) inputRef.current.value = '';
  }

  if (!canUpload) {
    // **入力欄を無効にして置くのではなく、理由を書きます。**
    // 押せない欄だけがあると、壊れているのか権限が無いのかが分かりません。
    //
    // **`<label>` は使いません** (レビュー指摘)。結び付ける相手が
    // `<p>` になり、ラベル付けできない要素を指すことになります ——
    // 関連付けが成立せず、読み上げから孤立します。
    return (
      <div className="field">
        <span className="field__label">{label}</span>
        <p className="muted">
          {uploadBlockedText[uploadBlockedBy]}
        </p>
        {hint !== undefined && <span className="field__hint">{hint}</span>}
      </div>
    );
  }

  return (
    <div className="field">
      <label className="field__label" htmlFor={inputId}>
        {label}
      </label>

      <input
        id={inputId}
        ref={inputRef}
        type="file"
        className="input"
        accept={accept}
        disabled={disabled === true || busy}
        aria-describedby={statusId}
        onChange={(e) => {
          const file = e.target.files?.[0];
          if (file !== undefined) void onPick(file);
        }}
      />

      {hint !== undefined && <span className="field__hint">{hint}</span>}

      {/*
        **進行と結果は読み上げにも流します。** 見た目だけの変化は、
        画面を見ていない利用者には何も起きていないのと同じになります。
      */}
      <p className="field__hint" id={statusId} aria-live="polite">
        {busy && 'アップロードしています...'}
        {!busy && value !== null && `添付しました (${value.width}×${value.height})`}
      </p>

      {error !== null && (
        <p className="alert alert--error" role="alert">
          {error}
        </p>
      )}

      {value !== null && (
        <div className="row">
          {/*
            next/image を使っていません。画像の配信元は環境ごとに変わり
            (ローカルの MinIO / 本番の CDN)、**最適化の経路に載せるには
            許可ホストを設定に固定する必要がある**ためです。
            ここは投稿前の確認用で、寸法も API が返す実寸を使えます。
          */}
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img
            className="attachment"
            src={value.url}
            width={value.width}
            height={value.height}
            alt="選択した画像のプレビュー"
          />
          <button
            type="button"
            className="btn btn--quiet"
            disabled={disabled === true || busy}
            onClick={() => {
              onChange(null);
              setError(null);
            }}
          >
            画像を外す
          </button>
        </div>
      )}
    </div>
  );
}

/**
 * アップロードの失敗を利用者向けの文言にします。
 *
 * **503 を「失敗」と書きません。** ストレージの設定が入っていないだけで、
 * 掲示板の閲覧・投稿・ログインは動きます (仕様書)。
 * 「画像だけが使えない」と伝わらないと、利用者は投稿そのものを諦めます。
 */
function describeUploadError(e: unknown): string {
  if (!(e instanceof ApiError)) {
    return 'アップロードできませんでした。通信環境を確かめてお試しください。';
  }
  switch (e.code) {
    case 'UNAUTHENTICATED':
      return 'ログインが必要です。ログインし直してからお試しください。';
    case 'PAYLOAD_TOO_LARGE':
      return 'サイズか画素数の上限 (5 MiB / 2500 万画素) を超えています。縮小してからお試しください。';
    case 'INVALID_ARGUMENT':
      return '受け付けられない形式です。JPEG / PNG / WebP のいずれかを選んでください。';
    case 'UNAVAILABLE':
      return 'いま画像の保管庫が使えません。画像なしでの投稿はできます。';
    default:
      return `アップロードできませんでした (${e.code})。`;
  }
}
