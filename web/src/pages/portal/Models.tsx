import { copyToast } from "@/lib/clipboard";
import { useMemo, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { Drawer } from "@/components/common/Drawer";
import { EmptyState } from "@/components/common/EmptyState";
import { EntityCell } from "@/components/common/EntityCell";
import { EntityIcon } from "@/components/common/EntityIcon";
import { FilterBar, type FilterField } from "@/components/common/FilterBar";
import { PageHead } from "@/components/common/PageHead";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { Icon } from "@/components/common/Icon";
import { ModelTagList } from "@/components/common/ModelTag";
import { ModalityTag } from "@/components/common/StatusTag";
import { usePricingQuery, useMeQuery } from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import { MODALITY_LABELS, billingModeLabel, modalityLabel } from "@/lib/format";
import { currentSymbol, discounted, getMoneyCtx, groupDiscountText, moneyOf } from "@/lib/money";
import { useSite } from "@/lib/site";
import type { ModelPrices, PricingModel, PricingVendor, SpecPrices } from "@/types";

/** 模态筛选胶囊（全部 + 冻结 8 类） */
const MOD_ITEMS: { key: string; label: string }[] = [
  { key: "", label: "全部" },
  ...Object.entries(MODALITY_LABELS).map(([k, label]) => ({ key: k, label })),
];

/**
 * 8 条 token lane 的展示顺序与文案（唯一一份）。
 *
 * 单位固定「每 1M tokens」：后端 `money.SumProducts(scale, ...)` 里
 * lane 单价就是「1 货币单位 / 1M tokens」的微值，换算是 Qty × Unit ÷ scale。
 * 门户上把单位写错（例如写成 /1K）会让用户按 1000 倍预估费用。
 */
const TOKEN_LANES: { key: keyof ModelPrices; label: string }[] = [
  { key: "input", label: "输入" },
  { key: "output", label: "输出" },
  { key: "cache", label: "缓存读取" },
  { key: "cache_create", label: "缓存写入" },
  { key: "cache_create_1h", label: "缓存写入 1h" },
  { key: "image", label: "图片输入" },
  { key: "audio", label: "音频输入" },
  { key: "audio_output", label: "音频输出" },
];

/** 模型接入端点：优先用后端 endpoints 补全 /v1 前缀，缺省按模态映射 */
function endpointOf(m: PricingModel): string {
  const ep = m.endpoints?.[0]?.replace(/^\//, "");
  if (ep) return ep.startsWith("v1/") ? `/${ep}` : `/v1/${ep}`;
  const byModality: Record<string, string> = {
    chat: "/v1/chat/completions",
    image: "/v1/images/generations",
    video: "/v1/videos",
    embedding: "/v1/embeddings",
    rerank: "/v1/rerank",
    moderation: "/v1/moderations",
    audio: m.id.startsWith("whisper") ? "/v1/audio/transcriptions" : "/v1/audio/speech",
  };
  return byModality[m.modality] ?? "/v1/chat/completions";
}

/** 按模态生成 curl 请求体 */
function bodyOf(m: PricingModel): string {
  switch (m.modality) {
    case "chat":
      return `{"model":"${m.id}","messages":[{"role":"user","content":"你好"}]}`;
    case "image":
      // 不写 size / n：两者都是**模型事实**（各模型卖的档位不同、图像一次只出 1 张）。
      // 写死 "size":"2K" 会让只卖 1K 的模型必然 400，而 "n":2 在任何图像端点都会被拒 ——
      // 示例代码是用户第一段照抄的东西，它必须能跑通。
      // 需要指定尺寸时看该模型文档的「尺寸与画幅」表。
      return `{"model":"${m.id}","prompt":"a cat"}`;
    case "video":
      // 轮询地址写 /v1/tasks/{id}：全站任务只有这一个读面（视频/音乐/图像同一套）。
      // 曾写 /v1/videos/{task_id} —— 那个端点从来不存在，照抄必然 404。
      return `{"model":"${m.id}","prompt":"城市夜景延时","seconds":"5","resolution":"1080p"}\n# 提交后轮询：GET /v1/tasks/{task_id}`;
    case "embedding":
      return `{"model":"${m.id}","input":"灵动 API"}`;
    case "rerank":
      return `{"model":"${m.id}","query":"排序依据","documents":["文档A","文档B"]}`;
    case "moderation":
      return `{"model":"${m.id}","input":"待审核文本"}`;
    case "audio":
      return m.id.startsWith("whisper")
        ? `# multipart/form-data 上传音频文件\n{"model":"${m.id}","file":"@audio.mp3"}`
        : `{"model":"${m.id}","input":"要合成的文本","voice":"alloy"}`;
    // 音乐/歌词都是异步任务：提交后拿 task_id 轮询，产物清单走 /artifacts。
    case "music":
      return `{"model":"${m.id}","prompt":"轻快的民谣，木吉他为主"}\n# 提交后轮询：GET /v1/tasks/{task_id}`;
    default:
      return `{"model":"${m.id}"}`;
  }
}

function curlOf(m: PricingModel, base: string): string {
  return `curl -X POST ${base}${endpointOf(m)} \\\n  -H "Authorization: Bearer $LD_API_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '${bodyOf(m)}'`;
}


/**
 * 价目渲染器：把「标价（微）」换成「我的价格（微）」。
 *
 * 后端 `/api/pricing` 下发的是**标价**（不打折），折扣只在扣费时按
 * `group_discount` 乘算。门户必须显示折后价，否则用户按标价预估、
 * 按折后价被扣，两边永远对不上（这正是"所见即所得"要消灭的那种差）。
 */
type Pricer = (micro: number | null | undefined) => string;

function makePricer(disc: number): Pricer {
  return (micro) => (micro === null || micro === undefined ? "—" : moneyOf(discounted(micro, disc)));
}

/** 档位表里最便宜的一档（按折后价排序，与用户实际支付顺序一致） */
function cheapest(map: Record<string, number> | undefined, disc: number): [string, number] | null {
  const entries = Object.entries(map ?? {})
    .filter(([, v]) => typeof v === "number" && Number.isFinite(v))
    .map(([k, v]) => [k, discounted(v, disc) ?? v] as [string, number])
    .sort((a, b) => a[1] - b[1]);
  return entries[0] ?? null;
}

/** 卡片价目摘要（按计费方式分支；只列**已配置**的 lane，未配置不占一行） */
function cardPriceRows(m: PricingModel, disc: number) {
  const price = makePricer(disc);
  const row = (label: string, value: string, key: string) => (
    <div className="row" key={key}>
      <span>{label}</span>
      <b className="mono">{value}</b>
    </div>
  );

  if (m.billing_mode === "tokens") {
    if (m.prices?.input == null) {
      return <div className="missing">未配置价格 · 调用将被拒绝</div>;
    }
    const rows: ReactNode[] = [row("输入", `${price(m.prices.input)} / 1M tokens`, "in")];
    // 输出是第二条必看项；其余 lane 只在运营真的配了独立价时才出现，
    // 否则卡片会变成 8 行相同数字（未配置 = 回落输入价，那是计费规则不是价目）。
    if (m.prices.output != null) {
      rows.push(row("输出", `${price(m.prices.output)} / 1M tokens`, "out"));
    }
    const extras = TOKEN_LANES.slice(2).filter((l) => m.prices[l.key] != null);
    if (extras.length > 0) {
      rows.push(
        row(
          extras.length === 1 ? extras[0].label : `另 ${extras.length} 条通道`,
          extras.length === 1
            ? `${price(m.prices[extras[0].key])} / 1M tokens`
            : "见完整价目",
          "extras",
        ),
      );
    }
    return rows;
  }

  if (m.billing_mode === "call") {
    if (m.prices?.call == null) {
      return <div className="missing">未配置价格 · 调用将被拒绝</div>;
    }
    return row("按次计费", `${price(m.prices.call)} / 次`, "call");
  }

  const sp = m.spec_prices;
  if (!sp || !sp.kind) return <div className="missing">价格矩阵类型未知</div>;
  if (sp.kind === "per_image") {
    const min = cheapest(sp.tier_price, disc);
    // 档位全空 = 真的没配价：必须说"会被拒绝"，不能只显示一行让人误以为只是档位限制。
    if (!min) {
      return <div className="missing">未配置价格 · 调用将被拒绝</div>;
    }
    // 「起」价按最低档展示，与默认档无关：默认档只决定「不传尺寸时按哪一档收」。
    const defTier = sp.resolution_fixed ?? "";
    const defUnit = defTier ? sp.tier_price?.[defTier] : undefined;
    return (
      <>
        {row("按图计费 · 起", `${moneyOf(min[1])} / 张`, "min")}
        <div className="row">
          <span className="muted">默认档（未指定尺寸）</span>
          <span className="mono">
            {defUnit !== undefined
              ? `${defTier} · ${price(defUnit)} / 张`
              : "未设置（需指定尺寸）"}
          </span>
        </div>
      </>
    );
  }
  if (sp.kind === "per_video") {
    const min = cheapest(sp.second_price, disc);
    if (!min) return <div className="missing">未配置价格 · 调用将被拒绝</div>;
    return (
      <>
        {min ? row("按秒计费 · 起", `${moneyOf(min[1])} / s`, "min") : null}
        <div className="row">
          <span className="muted">最低档</span>
          <span className="mono">{min?.[0] ?? "—"}</span>
        </div>
      </>
    );
  }
  // 音乐/歌词：按实际交付条数计费。必须写清"每条"，否则用户会按"一次任务"估算。
  if (sp.kind === "per_clip") {
    if (sp.per_clip_price == null) return <div className="missing">未配置价格 · 调用将被拒绝</div>;
    return row("按条计费", `${price(sp.per_clip_price)} / 条`, "clip");
  }
  // 音乐/歌词的存量口径：单一定价。缺价必须显式说"会被拒绝"，不能显示成 0 元。
  if (sp.kind === "per_task") {
    if (sp.per_task_price == null) return <div className="missing">未配置价格 · 调用将被拒绝</div>;
    return row("按任务计费", `${price(sp.per_task_price)} / 次`, "task");
  }
  // 音频按秒：单价 + 最低消费都要露出来，否则用户按"1 秒的价格"估算，
  // 实际被收最低消费时会认为平台多扣了钱。
  if (sp.kind === "per_second") {
    if (sp.per_second_price == null) return <div className="missing">未配置价格 · 调用将被拒绝</div>;
    return (
      <>
        {row("按秒计费", `${price(sp.per_second_price)} / 秒`, "sec")}
        {sp.min_price != null ? (
          <div className="row">
            <span className="muted">最低消费</span>
            <span className="mono">{price(sp.min_price)} / 次</span>
          </div>
        ) : null}
      </>
    );
  }
  return <div className="missing">价格矩阵类型未知</div>;
}

/** token lane 完整价目表（未配置的 lane 也列出来，并写明"按输入价"这条回落规则） */
function tokenTable(m: PricingModel, disc: number, symbol: string): ReactNode {
  const price = makePricer(disc);
  const input = m.prices?.input ?? null;
  return (
    <>
      <div className="price-matrix">
        <table>
          <thead>
            <tr>
              <th>计费通道</th>
              <th>单价（{symbol} / 1M tokens）</th>
            </tr>
          </thead>
          <tbody>
            {TOKEN_LANES.map((l) => {
              const v = m.prices?.[l.key];
              return (
                <tr key={l.key}>
                  <th>{l.label}</th>
                  <td>
                    {v != null ? (
                      price(v)
                    ) : l.key === "cache_create_1h" ? (
                      <span className="muted">按缓存写入价</span>
                    ) : (
                      <span className="muted">按输入价</span>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <div className="hint mt16">
        费用 = Σ(各类 Token 数 × 单价) ÷ 1M；
        未单独配置的通道**按输入价计**（缓存写入 1h 未配置时先回落缓存写入价，再回落输入价）。
        输入价 {input != null ? `${price(input)} / 1M` : "未配置"}。
      </div>
    </>
  );
}

/** 完整价目表 + 计费说明（详情抽屉） */
function fullPriceNode(m: PricingModel, disc: number, groupText: string) {
  const price = makePricer(disc);
  const symbol = currentSymbol();
  const table = (head: [string, string], body: ReactNode) => (
    <div className="price-matrix">
      <table>
        <thead>
          <tr>
            <th>{head[0]}</th>
            <th>{head[1]}</th>
          </tr>
        </thead>
        <tbody>{body}</tbody>
      </table>
    </div>
  );

  if (m.billing_mode === "tokens") {
    if (m.prices?.input == null) {
      return <div className="alert-banner danger">该模型未配置价格，调用将被拒绝。</div>;
    }
    return tokenTable(m, disc, symbol);
  }

  if (m.billing_mode === "call") {
    if (m.prices?.call == null) {
      return <div className="alert-banner danger">该模型未配置按次单价，调用将被拒绝。</div>;
    }
    const p = price(m.prices.call);
    return (
      <>
        {table(
          ["计费方式", "单价"],
          <tr>
            <th>按次计费</th>
            <td>{p} / 次</td>
          </tr>,
        )}
        <div className="hint mt16">
          每次成功调用固定扣费 {p}（已含分组折扣 {groupText}）。
        </div>
      </>
    );
  }

  const sp: SpecPrices | null = m.spec_prices;
  if (!sp || !sp.kind) return <div className="alert-banner danger">价格矩阵类型未知。</div>;

  if (sp.kind === "per_image") {
    return (
      <>
        {table(
          ["分辨率档位", "单价 / 张"],
          <>
            {Object.entries(sp.tier_price ?? {}).map(([tier, p]) => (
              <tr key={tier}>
                <th>{tier}</th>
                <td>{price(p)}</td>
              </tr>
            ))}
            <tr>
              <th>默认档（未指定尺寸）</th>
              <td>
                {sp.resolution_fixed && sp.tier_price?.[sp.resolution_fixed] !== undefined
                  ? `${sp.resolution_fixed} · ${price(sp.tier_price[sp.resolution_fixed])}`
                  : "未设置（需指定尺寸）"}
              </td>
            </tr>
          </>,
        )}
        <div className="hint mt16">
          未指定尺寸按默认档计（未设置默认档需显式指定尺寸）；写价目外的档位按最高档计。
          {sp.max_n ? `单次最多 ${sp.max_n} 张，` : ""}
          费用 = 档位单价 × 张数（已含分组折扣 {groupText}）。
        </div>
      </>
    );
  }

  if (sp.kind === "per_video") {
    const material = (label: string, micro: number | null | undefined, unit: string) => (
      <span key={label}>
        {label}：{micro == null ? "未配置" : `${price(micro)} / ${unit}`}
      </span>
    );
    // 参考视频两套口径互斥（保存期就会拒绝同时配置），展示时只会有一种成立。
    const refVideoSecond = sp.ref_video_second_price ?? {};
    const hasRefVideoSecond = Object.values(refVideoSecond).some((v) => typeof v === "number" && v > 0);
    const freeImages =
      sp.ref_image_free != null && sp.ref_image_free > 0 ? ` · 参考图前 ${sp.ref_image_free} 张免费` : "";
    const longText =
      sp.long_after_seconds != null && sp.long_video_multiplier != null
        ? `；时长 > ${sp.long_after_seconds} 秒时，时长费 ×${(sp.long_video_multiplier / getMoneyCtx().scale).toFixed(2)}`
        : "";
    return (
      <>
        {table(
          ["分辨率档位", "每秒单价"],
          Object.entries(sp.second_price ?? {}).map(([tier, p]) => (
            <tr key={tier}>
              <th>{tier}</th>
              <td>{price(p)} / s</td>
            </tr>
          )),
        )}
        {hasRefVideoSecond
          ? table(
              ["分辨率档位", "参考视频每秒单价"],
              Object.entries(refVideoSecond)
                .filter(([, v]) => typeof v === "number" && v > 0)
                .map(([tier, p]) => (
                  <tr key={tier}>
                    <th>{tier}</th>
                    <td>{price(p)} / s</td>
                  </tr>
                )),
            )
          : null}
        <div className="small muted mt16">
          {material("参考图", sp.ref_image_price, "张")}
          {" · "}
          {hasRefVideoSecond
            ? material("参考视频", refVideoSecond["720p"] ?? refVideoSecond["480p"], "秒")
            : material("参考视频", sp.ref_video_price, "条")}
          {" · "}
          {material("参考音频", sp.ref_audio_price, "条")}
          {freeImages}
        </div>
        <div className="hint mt16">
          费用 = 每秒单价[分辨率] × 时长 + 参考图单价 × max(0, 张数 − 免费张数) + 参考音频单价 × 个数
          {hasRefVideoSecond ? " + 参考视频每秒单价[分辨率] × 视频秒数" : " + 参考视频单价 × 条数"}
          {longText}，再乘分组折扣（{groupText}）；时长饱和到 [0, 3600] 秒。
        </div>
      </>
    );
  }

  // 音乐/歌词：按**实际交付条数**计费（kind=per_clip）。
  // 必须把「每条多少 / 最多几条 / 0 条免单」三件事一起写：只写单价，
  // 用户会按「一次任务 = 一条」估算，而冻结期是按上限占额度的。
  if (sp.kind === "per_clip") {
    if (sp.per_clip_price == null) {
      return <div className="alert-banner danger">该模型未配置每条单价，调用将被拒绝。</div>;
    }
    const unit = price(sp.per_clip_price);
    const cap = sp.max_n ?? 2;
    return (
      <>
        {table(
          ["计费方式", "单价"],
          <tr>
            <th>按条计费</th>
            <td>{unit} / 条</td>
          </tr>,
        )}
        <div className="small muted mt16">
          按<strong>实际交付条数</strong>结算（已含分组折扣 {groupText}）：一次任务交付 2 首收 2 条的钱，
          只交付 1 首收 1 条，一条都没交付则本次不计费。
          提交时按最多 {cap} 条预占额度，结算后立即释放差额。
        </div>
      </>
    );
  }
  // 音乐/歌词的**存量**口径：一次任务一个固定价（kind=per_task）。
  // 缺价时后端 perTaskCharge 直接拒绝，门户必须提前说清楚，
  // 而不是渲染成 "0 / 次" 让人以为免费。
  if (sp.kind === "per_task") {
    if (sp.per_task_price == null) {
      return <div className="alert-banner danger">该模型未配置按任务单价，调用将被拒绝。</div>;
    }
    const unit = price(sp.per_task_price);
    return (
      <>
        {table(
          ["计费方式", "单价"],
          <tr>
            <th>按任务计费</th>
            <td>{unit} / 次</td>
          </tr>,
        )}
        <div className="small muted mt16">
          每次任务固定扣费 {unit}（已含分组折扣 {groupText}）；一次任务产出多条结果（例如同时生成 2 首歌）
          也仍按一次计，不随产物条数放大。
        </div>
      </>
    );
  }
  // 音频按秒：必须说清"按时长 × 单价、不足最低消费按最低收"，
  // 否则用户按 1 秒的单价估算，被收最低消费时会认为平台多扣钱。
  if (sp.kind === "per_second") {
    if (sp.per_second_price == null) {
      return <div className="alert-banner danger">该模型未配置每秒单价，调用将被拒绝。</div>;
    }
    const unit = price(sp.per_second_price);
    const min = sp.min_price != null ? price(sp.min_price) : null;
    return (
      <>
        {table(
          ["计费方式", "单价"],
          <tr>
            <th>按秒计费</th>
            <td>{unit} / 秒</td>
          </tr>,
        )}
        <div className="small muted mt16">
          按生成音频的实际时长计费（已含分组折扣 {groupText}）
          {min ? `；单次不足 ${min} 按 ${min} 收取（起步计费时长）` : ""}
          {sp.max_seconds != null ? `；单次最多按 ${sp.max_seconds} 秒计费` : ""}。
        </div>
      </>
    );
  }
  return <div className="alert-banner danger">价格矩阵类型未知。</div>;
}

/**
 * 门户模型广场（S3）
 * 计费分组 / 关键词 / 模态 / 供应商筛选（移动端折叠进 Sheet）；模型卡片网格；
 * 卡片底部两个动作分别打开详情抽屉（完整价目 / 接入示例）。
 */
export function ModelsPage() {
  const pricing = usePricingQuery();
  const site = useSite();
  const me = useMeQuery();
  // 分组折扣只显「自己那一档」。公开端点已裁剪 group_discount 全表，
  // 这里的价目不能再让用户切换分组预览（那等于把全站折扣表暴露给未登录访客）。
  const group = me.data?.group ?? "default";
  // 未登录 / 后端未配置 → 不打折（= scale）。缺口径时按 scale 显示标价，
  // 绝不能按 0 显示「全部免费」。
  const disc = me.data?.group_discount ?? getMoneyCtx().scale;
  const groupText = groupDiscountText(disc);
  const allModels = pricing.data?.models ?? [];
  // 供应商胶囊只列「真的有在售模型」的分组。
  // 后端已按引用过滤过，这层是**防御**（后端若回退会冒出空分组胶囊），不是第二真相源。
  const vendors: PricingVendor[] = useMemo(() => {
    const used = new Set(allModels.map((m) => m.vendor));
    return (pricing.data?.vendors ?? []).filter((v) => used.has(v.id));
  }, [pricing.data, allModels]);
  const enabledModels = allModels;

  /** id → 供应商：卡片副标题按模型所属供应商补图标。逐张卡 `find` 是 O(n²)，
      模型广场一次能列出几十张卡。 */
  const vendorByID = useMemo(() => new Map(vendors.map((v) => [v.id, v])), [vendors]);

  const [kw, setKw] = useState("");
  const [modality, setModality] = useState("");
  const [vendor, setVendor] = useState("");
  /** 详情抽屉：原型两种入口（完整价目 / 接入示例）分别只展示对应区块 */
  const [detail, setDetail] = useState<{ model: PricingModel; mode: "price" | "curl" } | null>(null);

  const reset = () => {
    setKw("");
    setModality("");
    setVendor("");
  };

  const visible = useMemo(() => {
    const q = kw.trim().toLowerCase();
    return enabledModels.filter(
      (m) =>
        (!vendor || m.vendor === vendor) &&
        (!modality || m.modality === modality) &&
        (!q || m.id.toLowerCase().includes(q) || m.display_name.toLowerCase().includes(q)),
    );
  }, [enabledModels, vendor, modality, kw]);

  const vendorChips = (
    <SegmentedChips
      items={[
        { key: "", label: "全部供应商" },
        ...vendors.map((v) => ({ key: v.id, label: v.name })),
      ]}
      value={vendor}
      onChange={setVendor}
      renderItem={(it) => {
        const v = vendorByID.get(it.key);
        if (!v) return it.label;
        return (
          <>
            {/* 芯片里的图标用 xs：芯片高度只有一行文字，md 会把它撑成两行 */}
            <EntityIcon url={v.icon_url} name={v.name} color={v.color} size="xs" />
            {v.name}
          </>
        );
      }}
    />
  );

  const fields: FilterField[] = [
    { t: "search", id: "fKw", label: "关键词", placeholder: "搜索模型 ID / 名称" },
    { t: "btn", id: "reset", label: "重置筛选" },
  ];

  const hasChips = Boolean(modality || vendor || kw.trim());

  // 详情抽屉里的供应商行要带图标：价格页只给名字时，同名的代理商分组
  // （「OpenAI」官方 / 「OpenAI」中转）分不出来，图标才是一眼可辨的那一维。
  const detailVendor = detail ? vendorByID.get(detail.model.vendor) : undefined;

  const onFilterChange = (id: string, v: string) => {
    if (id === "fKw") setKw(v);
  };

  return (
    <>
      <PageHead
        title="模型广场"
        desc={`价格即为你的实付单价（分组 ${group} · ${groupText}）· 共 ${enabledModels.length} 款在售模型`}
      />

      <FilterBar
        fields={fields}
        value={{ fKw: kw }}
        onChange={onFilterChange}
        onReset={reset}
        extraActive={(modality ? 1 : 0) + (vendor ? 1 : 0)}
        chips={[
          {
            label: "模态",
            node: <SegmentedChips items={MOD_ITEMS} value={modality} onChange={setModality} />,
          },
          { label: "供应商", node: vendorChips },
        ]}
      />

      {pricing.isLoading ? (
        <div className="card">
          <EmptyState icon="◇" title="正在加载模型目录…" />
        </div>
      ) : pricing.isError ? (
        <div className="card">
          <EmptyState
            icon="⚠"
            title="模型目录加载失败"
            desc={pricing.error instanceof ApiError ? pricing.error.message : "请稍后重试"}
            action={
              <button type="button" className="btn btn-primary" onClick={() => void pricing.refetch()}>
                重试
              </button>
            }
          />
        </div>
      ) : !visible.length ? (
        <div className="card">
          <EmptyState
            icon="◇"
            title="该筛选组合下暂无模型"
            action={
              hasChips ? (
                <button type="button" className="btn" onClick={reset}>
                  清除筛选
                </button>
              ) : null
            }
          />
        </div>
      ) : (
        <div className="models-grid">
          {visible.map((m) => (
            <div key={m.id} className="model-card">
              {m.is_default ? (
                <span className="star" title="该模态的默认模型">
                  ★
                </span>
              ) : null}
              <div className="m-name">
                {/* m.icon_url 是后端算好的「自己的图标 or 所属供应商的图标」，前端不再拼回落链 */}
                <EntityIcon url={m.icon_url} name={m.display_name} color={m.vendor_color} size="sm" />
                <span className="m-title">{m.display_name}</span>
              </div>
              <div className="m-id">
                <code className="mono small">{m.id}</code>
                <button
                  type="button"
                  className="icon-btn"
                  aria-label={`复制模型 ID ${m.id}`}
                  title="复制模型 ID"
                  onClick={() => void copyToast(m.id)}
                >
                  <Icon name="copy" />
                </button>
              </div>
              <div className="desc">
                {/* 副标题也是身份信息：同名分组（官方 / 中转）只看文字分不出来 */}
                <EntityCell
                  iconUrl={vendorByID.get(m.vendor)?.icon_url}
                  name={m.vendor_name}
                  color={m.vendor_color}
                  size="xs"
                >
                  <span>· {modalityLabel(m.modality)}</span>
                </EntityCell>
              </div>
              <div className="chips">
                {/* 标签放徽章行而不是名称行：`.m-name` 有 padding-right 给 ★ 让位、
                    标题是 ellipsis，塞进去会把模型名压成两三个字。 */}
                <ModelTagList tags={m.tags} max={2} />
                <ModalityTag modality={m.modality} />
                <span className="badge badge-gray">{billingModeLabel(m)}</span>
                {/* 免费模型是运营显式声明的事实（P0-04）：不标出来，用户会以为「价格显示 0」是页面出错 */}
                {m.is_free ? <span className="badge badge-success">免费</span> : null}
              </div>
              {m.is_default ? (
                <div className="ep-tags">
                  <span>默认</span>
                </div>
              ) : null}
              <div className="price-rows">{cardPriceRows(m, disc)}</div>
              <div className="link-row">
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={() => setDetail({ model: m, mode: "price" })}
                >
                  完整价目
                </button>
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={() => setDetail({ model: m, mode: "curl" })}
                >
                  接入示例
                </button>
              </div>
            </div>
          ))}
        </div>
      )}

      <Drawer
        open={detail !== null}
        onClose={() => setDetail(null)}
        // 图标只进来 `titleIcon`、不进 title 字符串：抽屉的可访问名取 title，
        // 换成 ReactNode 会让 e2e 与读屏都只能读到"详情抽屉"。
        titleIcon={
          detail
            ? {
                url: detail.model.icon_url,
                color: detail.model.vendor_color,
                name: detail.model.display_name || detail.model.id,
              }
            : undefined
        }
        title={
          detail ? `${detail.mode === "price" ? "完整价目" : "接入示例"} · ${detail.model.id}` : ""
        }
      >
        {detail?.mode === "price" ? (
          <>
            {detail.model.tags?.length ? (
              <div className="drawer-tags">
                <ModelTagList tags={detail.model.tags} />
              </div>
            ) : null}
            <div className="kv-grid">
              <div className="kv">
                <div className="k">供应商</div>
                <div className="v">
                  <EntityCell
                    iconUrl={detailVendor?.icon_url}
                    name={detail.model.vendor_name}
                    color={detailVendor?.color}
                    size="xs"
                  />
                </div>
              </div>
              <div className="kv">
                <div className="k">模态</div>
                <div className="v">{modalityLabel(detail.model.modality)}</div>
              </div>
              <div className="kv">
                <div className="k">计费方式</div>
                <div className="v">{billingModeLabel(detail.model)}</div>
              </div>
              <div className="kv">
                <div className="k">我的分组</div>
                <div className="v">
                  {group} · {groupText}
                </div>
              </div>
              <div className="kv">
                <div className="k">默认模型</div>
                <div className="v">{detail.model.is_default ? "是" : "否"}</div>
              </div>
            </div>

            <div className="blk-title">价格明细</div>
            {fullPriceNode(detail.model, disc, groupText)}
          </>
        ) : detail ? (
          <>
            <div className="code-block">
              <button
                type="button"
                className="copy"
                onClick={() => void copyToast(curlOf(detail.model, site.apiBase))}
              >
                复制
              </button>
              {curlOf(detail.model, site.apiBase)}
            </div>
            <div className="hint mt16">
              把 <span className="mono">$LD_API_KEY</span> 换成你的密钥；视频任务先提交，再到调用记录查看进度。
            </div>
          </>
        ) : null}
      </Drawer>
    </>
  );
}
