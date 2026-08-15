// 管理画面で共有する見た目。
//
// CSS フレームワークを入れず、既存の page.tsx と同じインラインスタイルに
// 揃えています。**画面が 3 枚のうちに依存を増やさない**ためで、
// 増えたら CSS Modules なり Tailwind なりへ移す判断をやり直します。
import type { CSSProperties } from 'react';

export const colors = {
  bg: '#000',
  fg: '#00f',
  dim: '#55f',
  border: '#00f',
  danger: '#f55',
  warn: '#fa0',
} as const;

export const page: CSSProperties = {
  backgroundColor: colors.bg,
  color: colors.fg,
  minHeight: '100vh',
  padding: '2rem',
  fontFamily: 'monospace',
};

export const heading: CSSProperties = {
  borderBottom: `1px solid ${colors.border}`,
  paddingBottom: '0.5rem',
};

export const card: CSSProperties = {
  margin: '1rem 0',
  padding: '1rem',
  border: `1px dashed ${colors.border}`,
};

export const button: CSSProperties = {
  backgroundColor: 'transparent',
  color: colors.fg,
  border: `1px solid ${colors.border}`,
  fontFamily: 'monospace',
  padding: '0.3rem 0.8rem',
  cursor: 'pointer',
  marginRight: '0.5rem',
};

// 取り消せない操作は色を変える。論理削除なので DB からは戻せるが、
// **戻す UI は無い** (ADR 0011 の引き受けるコスト)。
export const dangerButton: CSSProperties = {
  ...button,
  color: colors.danger,
  borderColor: colors.danger,
};

export const input: CSSProperties = {
  backgroundColor: colors.bg,
  color: colors.fg,
  border: `1px solid ${colors.border}`,
  fontFamily: 'monospace',
  padding: '0.3rem',
};

export const label: CSSProperties = {
  display: 'block',
  margin: '0.8rem 0 0.3rem',
  color: colors.dim,
};

/**
 * 日時の整形。
 *
 * **タイムゾーンを明示します。** 既定はブラウザ / コンテナ任せで、
 * `TZ` を指定していない環境では UTC になり 9 時間ずれます
 * (既存の page.tsx と同じ理由)。
 */
export function formatTime(iso: string): string {
  return new Date(iso).toLocaleString('ja-JP', { timeZone: 'Asia/Tokyo' });
}
