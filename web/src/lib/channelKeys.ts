/**
 * 密钥池文本解析的**唯一入口**（新建渠道的批量录入、密钥池面板的批量添加/整池重建共用）。
 *
 * 为什么必须集中：这四处都要回答同样四个问题 —— 哪几行是密钥、哪些行是噪音、
 * 哪些行重复、超没超规格。各写一份的结果是"新建时能拦住、面板里拦不住"，
 * 而拦不住的那一处正好是运营改线上渠道的地方。
 *
 * 三条与后端逐字对齐的规格（`pkg/channelkeys`）：
 *   - 单条 ≤ 1024 字符；
 *   - 单渠道 ≤ 100 把；
 *   - 密钥内部不允许空白（opaque token，出现空白就是粘贴错位）。
 */

/** 单渠道密钥数上限（与后端 `channelkeys.MaxKeysPerChannel` 一致）。 */
export const MAX_KEYS = 100;

/** 单条密钥长度上限（与后端 `channelkeys.MaxKeyLen` 一致）。 */
export const MAX_KEY_LEN = 1024;

export interface KeyLineIssue {
  /** 行号（从 1 开始）；0 = 整批问题（如超过 100 把） */
  line: number;
  /** 该行原文（已 trim，过长会截断用于展示） */
  text: string;
  /** 给人看的原因 */
  message: string;
  /**
   * true = 阻断提交；false = 只是提示（例如重复行被自动跳过）。
   *
   * 由本模块判定而不是让 UI 各自判断：同一个 issue 在一处拦、在另一处放过，
   * 就会出现"面板能提交、新建提交不了"这种自相矛盾的行为。
   */
  blocking: boolean;
}

export interface ParsedKeys {
  /** 去重、去空之后可直接提交的密钥（保序） */
  keys: string[];
  issues: KeyLineIssue[];
}

/** 形似后端返回的指纹（`sk-abc…wxyz` / `sk-abc****wxyz`）。 */
function looksMasked(key: string): boolean {
  return key.includes("…") || key.includes("...") || /\*{3,}/.test(key);
}

/** 把粘贴内容解析成密钥数组。 */
export function parseKeyLines(raw: string): ParsedKeys {
  const keys: string[] = [];
  const issues: KeyLineIssue[] = [];
  /** 明文 → 首次出现的行号（报"与第 N 行重复"，而不是只报"重复"） */
  const firstSeen = new Map<string, number>();
  // 只按 \n 切：密钥是 opaque token，按 `,;` 切会把它静默截断成两把废 key，
  // 而报错只会是 401（排查方向跑到"账号被封"）。行尾残留的 \r 由 trim 吃掉。
  const lines = raw.split("\n");

  for (let i = 0; i < lines.length; i += 1) {
    const text = lines[i].trim();
    const line = i + 1;
    // 空行是粘贴常态，不产生 issue：噪音会淹没真正的问题行。
    if (!text) continue;

    if (looksMasked(text)) {
      issues.push({ line, text, blocking: true, message: "这是脱敏后的指纹，请填完整密钥" });
      continue;
    }
    if (text.length > MAX_KEY_LEN) {
      issues.push({
        line,
        text: text.slice(0, 32) + "…",
        blocking: true,
        message: `超过 ${MAX_KEY_LEN} 字符（当前 ${text.length}）`,
      });
      continue;
    }
    if (/\s/.test(text)) {
      issues.push({ line, text: text.slice(0, 32) + "…", blocking: true, message: "密钥里不该有空格或制表符" });
      continue;
    }
    const first = firstSeen.get(text);
    if (first !== undefined) {
      // 重复不阻断（后端也按 keyID 幂等），但要告诉运营"这一行不会被添加"，
      // 否则他会以为粘贴了 5 把、实际只进去 3 把。
      issues.push({
        line,
        text: text.slice(0, 32) + "…",
        blocking: false,
        message: `与第 ${first} 行重复，将跳过`,
      });
      continue;
    }
    firstSeen.set(text, line);
    keys.push(text);
  }

  if (keys.length > MAX_KEYS) {
    issues.push({
      line: 0,
      text: "",
      blocking: true,
      message: `最多 ${MAX_KEYS} 把，当前 ${keys.length} 把`,
    });
  }
  return { keys, issues };
}

/** 取第一条阻断性问题（表单提交前给一句能直接 pop 的话）。 */
export function firstBlockingIssue(issues: KeyLineIssue[]): KeyLineIssue | null {
  return issues.find((x) => x.blocking) ?? null;
}
