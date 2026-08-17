// 例外を画面に出せる形にする。
//
// **分岐は `code` で行います。** `message` は人間向けで予告なく変わります
// (api/openapi.yaml の Error スキーマ)。文言で分岐すると、
// API 側が言い回しを直しただけで画面の挙動が変わります。
import { ApiError } from './api';

/**
 * 例外を 1 行にします。
 *
 * **`ApiError` は `code` を添えます。** 文言は変わるので、
 * 問い合わせを受けたときに手がかりになるのはコードのほうです。
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

/** 指定のエラーコードかどうか。 */
export function isCode(e: unknown, code: ApiError['code']): boolean {
  return e instanceof ApiError && e.code === code;
}

/**
 * 書き込みの失敗を、利用者向けの文言にします。
 *
 * **どの画面でも同じ意味になるものだけをここに置きます。**
 * 画面固有の言い換え (「この画像は添付できません」など) は
 * 呼び出し側で先に分岐してください。
 *
 * 403 を「入力の誤り」と書かないのが要点です —— 状態変更メソッドの
 * `Origin` 検証の失敗がここに来るため (ADR 0013 決定 1)、
 * 利用者は文面を直し続けることになります。
 */
export function describeWriteError(e: unknown): string {
  if (!(e instanceof ApiError)) {
    return '通信できませんでした。接続を確かめて、もう一度お試しください。';
  }
  switch (e.code) {
    case 'UNAUTHENTICATED':
      return 'ログインの有効期限が切れています。ログインし直してください。';
    case 'PERMISSION_DENIED':
      return '操作が拒否されました。ページを開き直してからお試しください。';
    case 'INVALID_ARGUMENT':
      return `入力を確認してください: ${e.message}`;
    case 'NOT_FOUND':
      return '対象が見つかりません。すでに削除されている可能性があります。';
    case 'RESOURCE_EXHAUSTED':
      return '短時間に送りすぎています。しばらく時間をおいてからお試しください。';
    case 'PAYLOAD_TOO_LARGE':
      return 'サイズが大きすぎます。小さくしてからお試しください。';
    case 'UNAVAILABLE':
      return 'この機能はいま使えません。時間をおいてお試しください。';
    default:
      return `処理できませんでした (${e.code})。時間をおいてもう一度お試しください。`;
  }
}
