'use client';

// 投稿に添付された画像。
//
// 【消えた画像を「読み込み失敗」に見せない】
// モデレーターに削除された画像も、投稿には**行として残ります** ——
// `image` は返るが `url` は 404 になります。行を残すのは
// 「画像は削除されました」と「元から画像なし」を区別するためです
// (ADR 0016 問題 3)。区別している以上、画面でも区別して出します。
//
// 回収バッチと CDN のキャッシュには遅れがあるので、
// **削除直後はまだ見える**ことがあります (ADR 0011 の未解決)。
import { useState } from 'react';

import type { Image } from '../lib/api';

export function Attachment({ image, alt }: { image: Image; alt: string }) {
  const [broken, setBroken] = useState(false);

  if (broken) {
    return (
      <p className="alert alert--warn">
        この画像は表示できません。削除された可能性があります。
      </p>
    );
  }

  return (
    // next/image を使っていません (配信元が環境ごとに変わり、
    // 最適化の経路には許可ホストの固定が要るため。ADR 0007 決定 5)。
    //
    // **`width` / `height` は API が返す実寸です。** 入れておくと
    // 読み込み前に場所が確保され、あとから本文が飛びません。
    // eslint-disable-next-line @next/next/no-img-element
    <img
      className="attachment"
      src={image.url}
      width={image.width}
      height={image.height}
      alt={alt}
      loading="lazy"
      decoding="async"
      onError={() => setBroken(true)}
    />
  );
}
