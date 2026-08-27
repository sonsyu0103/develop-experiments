// 画面で共有する小道具。
//
// 【見た目はここに置かなくなりました】
// 以前はここが CSSProperties のオブジェクトを配っていました。
// 画面が 3 枚のうちは依存を増やさない判断として妥当でしたが、Phase 3 で
// 画面が 8 枚になり、**インラインスタイルでは書けないもの**
// (`:focus-visible` / `:hover` / メディアクエリ / `prefers-reduced-motion`)
// が欠落として残っていました。見た目は `app/globals.css` に移してあります。
//
// 依存は増やしていません —— Next.js が標準で持つグローバル CSS だけです。

/**
 * 日時の整形。
 *
 * **タイムゾーンを明示します。** 既定はブラウザ / コンテナ任せで、
 * `TZ` を指定していない環境では UTC になり 9 時間ずれます。
 */
export function formatTime(iso: string): string {
  return new Date(iso).toLocaleString('ja-JP', { timeZone: 'Asia/Tokyo' });
}

/**
 * `<time datetime="...">` に入れる値。
 *
 * **表示用の文字列とは別物です。** 整形後の「2026/8/17 12:00:00」は
 * 機械可読ではないので、支援技術や検索エンジンにはこちらを渡します。
 */
export function machineTime(iso: string): string {
  return new Date(iso).toISOString();
}

/**
 * クラス名を組み立てます。`false` / `undefined` は落とします。
 *
 * 条件付きのクラスを付けるたびにテンプレートリテラルを書くと、
 * 空白の付け忘れで**クラス名が連結して静かに効かなくなります**。
 */
export function cx(...parts: Array<string | false | null | undefined>): string {
  return parts.filter((p): p is string => typeof p === 'string' && p !== '').join(' ');
}
