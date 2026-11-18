import { useEffect, useState } from "react";
import { Dialog } from "@/components/common/Dialog";
import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { MoneyInput } from "@/components/common/MoneyInput";
import { SwitchRow } from "@/components/common/SwitchRow";
import { ApiError } from "@/lib/apiClient";
import { formatMoney } from "@/lib/format";
import type { AgentSettings, AgentStats } from "@/lib/api/agent";
import { useSaveAgentSettingsMutation } from "@/lib/api/agent";

export interface AgentSettingsDialogProps {
  open: boolean;
  onClose: () => void;
  settings?: AgentSettings;
  stats?: AgentStats;
  statsFailed?: boolean;
}

function num(v: string): number {
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

/** 区间兜底：接口没回来时用它，值必须与后端 model 包的常量表一致（变化时只改这一处）。 */
const FALLBACK_LIMITS: AgentSettings["limits"] = {
  max_steps: [1, 40],
  max_tokens_per_run: [1000, 20_000_000],
  context_window: [32_768, 10_000_000],
  max_output_tokens: [256, 32_768],
};

/**
 * 运行参数 + 运行统计。
 *
 * 为什么这些必须"显式保存"而不是即改即存：它们是**全站参数**
 * （关掉总闸所有人立刻用不了助手、改步数会影响每一轮成本），
 * 一个误触就是线上事故；而素材、模型是"本轮作用域"，即时生效才是对的。
 *
 * 三个 token 旋钮的语义不能混：
 *   - 单轮最多消耗 Token：成本闸，**跨步累加**，0 = 不限；
 *   - 上下文窗口：输入预算，决定"这次能带多少历史"，0 = 用内置默认；
 *   - 单次输出上限：下发给上游的 max_tokens，0 = 用内置默认。
 */
export function AgentSettingsDialog({ open, onClose, settings, stats, statsFailed }: AgentSettingsDialogProps) {
  const save = useSaveAgentSettingsMutation();
  const [form, setForm] = useState({
    enabled: true,
    max_steps: 12,
    max_tokens_per_run: 2_000_000,
    context_window: 0,
    max_output_tokens: 0,
    /** 每日成本上限（微）；null = 空（提交成 0 = 不限） */
    daily_amount_budget: null as number | null,
    dry_run_required: true,
  });
  const [err, setErr] = useState("");
  const [ok, setOk] = useState("");

  useEffect(() => {
    if (!settings) return;
    setForm({
      enabled: settings.item.enabled,
      max_steps: settings.item.max_steps,
      max_tokens_per_run: settings.item.max_tokens_per_run,
      context_window: settings.item.context_window ?? 0,
      max_output_tokens: settings.item.max_output_tokens ?? 0,
      daily_amount_budget: settings.item.daily_amount_budget || null,
      dry_run_required: settings.item.dry_run_required,
    });
  }, [settings]);

  // 每次打开都清掉上一次的提示：关掉再打开还留着"已保存"会让人以为这次也存了。
  useEffect(() => {
    if (open) {
      setErr("");
      setOk("");
    }
  }, [open]);

  const limits = settings?.limits ?? FALLBACK_LIMITS;

  /**
   * 提交前校验（与后端 ValidateSettings 同规则）。
   *
   * 为什么不能只靠 HTML 的 min/max：手输数字可以绕过（也能粘贴），
   * 而绕过后端会回一句 400 —— 用户已经填完整张表单才被拒，体验最差。
   */
  const validate = (): string => {
    const [lo, hi] = limits.max_steps;
    if (form.max_steps < lo || form.max_steps > hi) {
      return `单轮步数只能是 ${lo}–${hi}`;
    }
    const [tlo, thi] = limits.max_tokens_per_run;
    if (form.max_tokens_per_run !== 0 && (form.max_tokens_per_run < tlo || form.max_tokens_per_run > thi)) {
      return `单轮 Token 预算只能是 0（不限）或 ${tlo}–${thi}`;
    }
    const [clo, chi] = limits.context_window;
    if (form.context_window !== 0 && (form.context_window < clo || form.context_window > chi)) {
      return `上下文窗口只能是 0（默认）或 ${clo}–${chi}`;
    }
    const [olo, ohi] = limits.max_output_tokens;
    if (form.max_output_tokens !== 0 && (form.max_output_tokens < olo || form.max_output_tokens > ohi)) {
      return `单次输出上限只能是 0（默认）或 ${olo}–${ohi}`;
    }
    return "";
  };

  const submit = () => {
    setErr("");
    setOk("");
    const bad = validate();
    if (bad) {
      setErr(bad);
      return;
    }
    save.mutate(
      {
        enabled: form.enabled,
        max_steps: form.max_steps,
        max_tokens_per_run: form.max_tokens_per_run,
        context_window: form.context_window,
        max_output_tokens: form.max_output_tokens,
        // 空 = 不限（0）。字段名必须是 daily_amount_budget：
        // 曾经前端发 daily_quota_budget，后端忽略未知键 → "提示已保存但没落库"。
        daily_amount_budget: form.daily_amount_budget ?? 0,
        dry_run_required: form.dry_run_required,
      },
      {
        onSuccess: () => setOk("已保存"),
        onError: (e) => setErr(e instanceof ApiError ? e.message : "保存失败，请重试"),
      },
    );
  };

  return (
    <Dialog
      portal
      layer={2}
      open={open}
      onClose={onClose}
      title="运行参数"
      size="lg"
      actions={[
        { label: "关闭", onClick: onClose },
        { label: save.isPending ? "保存中…" : "保存", variant: "primary", onClick: submit, disabled: save.isPending },
      ]}
    >
      <div className="agent-dlg" data-testid="agent-settings">
        {settings && !settings.available ? (
          <div className="agent-alert agent-alert--warn" data-testid="agent-unavailable">
            {settings.hint || "请先在模型管理配置一个可用的对话模型"}
          </div>
        ) : null}

        <div className="form-grid">
          <Field label="启用接入助手">
            <SwitchRow
              label=""
              ariaLabel="启用接入助手"
              testId="agent-enabled"
              checked={form.enabled}
              onChange={(v) => setForm((f) => ({ ...f, enabled: v }))}
            />
          </Field>

          <Field
            label="单轮最多调用步数"
            htmlFor="agent-max-steps"
            tip="一次提问里最多调用几次工具。越大越能做完复杂接入，成本也越高"
          >
            <Input
              id="agent-max-steps"
              type="number"
              min={limits.max_steps[0]}
              max={limits.max_steps[1]}
              value={form.max_steps}
              onChange={(e) => setForm((f) => ({ ...f, max_steps: num(e.target.value) }))}
              data-testid="agent-max-steps"
            />
          </Field>

          <Field
            label="单轮最多消耗 Token"
            htmlFor="agent-max-tokens"
            tip="这一轮所有步骤的 token 总和上限（每步都要重付整份上下文）。0 = 不限"
          >
            <Input
              id="agent-max-tokens"
              type="number"
              min={0}
              step={1000}
              value={form.max_tokens_per_run}
              onChange={(e) => setForm((f) => ({ ...f, max_tokens_per_run: num(e.target.value) }))}
              data-testid="agent-max-tokens"
            />
          </Field>

          <Field
            label="上下文窗口"
            htmlFor="agent-ctx-window"
            tip="一次请求能带多少历史。0 = 用默认值；模型页填了更小的值时会以模型为准"
          >
            <Input
              id="agent-ctx-window"
              type="number"
              min={0}
              step={1000}
              value={form.context_window}
              onChange={(e) => setForm((f) => ({ ...f, context_window: num(e.target.value) }))}
              data-testid="agent-ctx-window"
            />
          </Field>

          <Field
            label="单次输出上限"
            htmlFor="agent-max-output"
            tip="每次回复最多生成多少 token。0 = 用默认值；上游硬限制更小时以上游为准"
          >
            <Input
              id="agent-max-output"
              type="number"
              min={0}
              step={256}
              value={form.max_output_tokens}
              onChange={(e) => setForm((f) => ({ ...f, max_output_tokens: num(e.target.value) }))}
              data-testid="agent-max-output"
            />
          </Field>

          <Field
            label="每日成本上限"
            htmlFor="agent-daily-budget"
            tip="按估算成本累计，超过后当天不再接受新一轮。留空 = 不限"
          >
            <MoneyInput
              id="agent-daily-budget"
              value={form.daily_amount_budget}
              onChange={(v) => setForm((f) => ({ ...f, daily_amount_budget: v }))}
              placeholder="不限"
              aria-label="每日成本上限"
            />
          </Field>

          <Field label="新建渠道必须探测通过">
            <SwitchRow
              label=""
              ariaLabel="新建渠道必须探测通过"
              checked={form.dry_run_required}
              onChange={(v) => setForm((f) => ({ ...f, dry_run_required: v }))}
            />
          </Field>
        </div>

        {settings ? (
          <div className="agent-dlg__row">
            <span>今日已用</span>
            {/* 后端下发的是**微**：直接 toLocaleString 会放大 100 万倍 */}
            <b>{formatMoney(settings.today_spend)}</b>
          </div>
        ) : null}

        {err ? <div className="agent-alert agent-alert--bad">{err}</div> : null}
        {ok ? <div className="agent-alert agent-alert--ok">{ok}</div> : null}

        {stats ? (
          <div className="agent-stats" data-testid="agent-stats">
            <span>近 {stats.days} 天：{stats.runs} 轮</span>
            <span className="agent-dot">失败 {stats.failed}</span>
            <span className="agent-dot">提案 {stats.proposals}</span>
            <span className="agent-dot">已生效 {stats.applied}</span>
            <span className="agent-dot">估算成本 {formatMoney(stats.quota_est)}</span>
            {stats.avg_latency_ms > 0 ? (
              <span className="agent-dot">平均 {(stats.avg_latency_ms / 1000).toFixed(1)} 秒</span>
            ) : null}
          </div>
        ) : statsFailed ? (
          // 静默不渲染是错的：这一行本来就是给运营看"花了多少"，
          // 消失了会被当成"还没花钱"。明说取不到，并强调不影响对话。
          <div className="agent-alert agent-alert--warn" data-testid="agent-stats-failed">
            运行统计暂时取不到，不影响对话
          </div>
        ) : null}
      </div>
    </Dialog>
  );
}
