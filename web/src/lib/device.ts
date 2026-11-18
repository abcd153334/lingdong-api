/**
 * 会话设备标识（登录设备列表用）。
 *
 * 只做「让人认出是哪台设备」这一件事，不追求完整 UA 解析：
 * 完整解析要引一个几十 KB 的库，而设备列表的用途是**撤销**，
 * 用户只需要区分「这是不是我」。
 */

/** UA → 「浏览器 · 系统」；认不出就整体回落，绝不返回空串（空串在列表里是一行空白） */
export function uaLabel(ua: string): string {
  const s = (ua || "").trim();
  if (!s) return "未知设备";
  return `${browserOf(s)} · ${osOf(s)}`;
}

/**
 * 浏览器判定顺序不能改。
 *
 * 真实 UA 全是互相包含的：Edge 含 "Chrome/"、Chrome 含 "Safari/"、
 * iOS 上的 Chrome 还含 "CriOS"。所以必须先判「最像的」，否则
 * 全世界都会显示成 Safari（且不报错，只是永远认错）。
 */
function browserOf(s: string): string {
  if (/Edg\//.test(s)) return "Edge";
  if (/OPR\/|Opera/.test(s)) return "Opera";
  if (/MicroMessenger/.test(s)) return "微信";
  if (/Firefox\/|FxiOS/.test(s)) return "Firefox";
  if (/CriOS/.test(s)) return "Chrome";
  if (/Chrome\//.test(s)) return "Chrome";
  if (/Safari\//.test(s)) return "Safari";
  if (/curl\/|Wget|python-requests|Go-http-client|okhttp/i.test(s)) return "命令行";
  return "未知浏览器";
}

function osOf(s: string): string {
  // Android 的 UA 里也带 "Linux"，iOS 的也带 "Mac OS X"，故手机系统必须先判
  if (/iPhone|iPad|iPod/.test(s)) return "iOS";
  if (/Android/.test(s)) return "Android";
  if (/Windows NT/.test(s)) return "Windows";
  if (/Mac OS X|Macintosh/.test(s)) return "macOS";
  if (/CrOS/.test(s)) return "ChromeOS";
  if (/Linux/.test(s)) return "Linux";
  return "未知系统";
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/**
 * 相对时间（「3 分钟前」）。
 *
 * `now` 可注入：否则单测只能靠 mock 全局 Date，而真跑时又会在
 * 跨秒边界上偶发翻红。未来时间（时钟漂移）按「刚刚」处理 ——
 * 显示「-2 分钟前」比不精确更让人困惑。
 */
export function timeAgo(iso: string, now: number = Date.now()): string {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return "—";
  const d = now - t;
  if (d < MINUTE) return "刚刚";
  if (d < HOUR) return `${Math.floor(d / MINUTE)} 分钟前`;
  if (d < DAY) return `${Math.floor(d / HOUR)} 小时前`;
  if (d < 30 * DAY) return `${Math.floor(d / DAY)} 天前`;
  return new Date(t).toLocaleDateString();
}
