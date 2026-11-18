/**
 * 发码按钮的交替文案（全站唯一）。
 *
 * 为什么要有这个函数：同一个按钮在「发送 / 发送中 / 倒计时」三态之间切换，
 * 文案原先散在四个页面里各写一份三目，已经漂移（有的写「重新发送」、
 * 有的写「重新发送(60s)」、有的干脆没有 pending 态）。
 *
 * 约束：三种文案的渲染宽度都必须 ≤ 按钮的 `min-width: 120px`，
 * 否则切换时按钮变宽、左侧输入区跟着跳。改动文案前先量宽度。
 */
export function sendCodeText(
  left: number,
  pending: boolean,
  idle = "发送验证码",
): string {
  // pending 优先：发送途中即使倒计时已经开始，也要先告诉用户"在发"。
  if (pending) return "发送中…";
  if (left > 0) return `重新发送(${left}s)`;
  return idle;
}
