import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";

import { Card } from "@/components/common/Card";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { EmptyState } from "@/components/common/EmptyState";
import { EntityIcon } from "@/components/common/EntityIcon";
import { Field } from "@/components/common/Field";
import { StatCard } from "@/components/common/StatCard";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Tag } from "@/components/common/StatusTag";
import { Textarea } from "@/components/common/Textarea";
import { TrendChart } from "@/components/common/Charts";
import { toastOk } from "@/components/common/Toaster";
import { toastApiError } from "@/lib/apiToast";
import { useAdminAppStatsQuery, useSetAppMaintenanceMutation } from "@/lib/api";
import { formatCent, formatDateTime, formatMoney } from "@/lib/format";
import type { AdminAppItem } from "@/types";

import { hasError, runeLen, validateMaintenance, type FormErrors } from "../validation";

const ONLINE_TEXT = { online: "在线", idle: "空闲", offline: "离线" } as const;
const ONLINE_TONE = { online: "success", idle: "warning", offline: "gray" } as const;

/** 运行态的一行指标：标签左、等宽数值右（同一列对齐，扫读比四张卡快）。 */
function RunRow({ label, value, sub }: { label: string; value: ReactNode; sub?: ReactNode }) {
  return (
    <div className="run-row">
      <span className="run-row__lbl">{label}</span>
      <span className="run-row__val">
        {value}
        {sub ? <span className="run-row__sub">{sub}</span> : null}
      </span>
    </div>
  );
}

/**
 * 概览（应用仪表盘 + 运行态 + 维护模式）。
 *
 * 「在线」页签已并入本页（2026-11-20）：两页读的是**同一条** `/stats`，
 * 分开只会让运营在两个页签之间来回核对「在线没在线 / 昨天调了多少」。
 * 现在一屏内按「是谁 → 经营多少 → 现在跑得怎么样 → 怎么干预」的顺序读：
 *
 *   ① 应用名片：身份、状态徽标、进入密钥/编辑的入口（顶栏没有标题，这里就是标题）；
 *   ② 四格经营指标：全部来自**列表行**（`app`）—— 卡片显示的就是它们，弹窗与卡片
 *      不可能对不上，也不阻塞在统计接口上；
 *   ③ 趋势图 + 运行态：都来自 `/stats`，一个看走势、一个看当下；
 *   ④ 维护模式：只拦**存量流量**（/v1 返回 503），不影响签发；
 *   ⑤ 应用状态：停用只拦**新签发**，已签发的密钥照常可用并继续扣费。
 *
 * ④ 与 ⑤ 的分工是这一段最重要的语义，两个方向都必须写清楚 ——
 * 一次操作不该同时完成两件事，也不该让人以为"停用 = 断流"。
 */
export function OverviewTab({
  app,
  onToggleStatus,
  onEdit,
  onOpenKeys,
  statusBusy,
  onDirtyChange,
}: {
  app: AdminAppItem;
  onToggleStatus: () => void;
  onEdit: () => void;
  /** 打开「应用密钥」抽屉：弹窗没有底栏了，"查看密钥"落在这里（任何情况都到得了）。 */
  onOpenKeys: () => void;
  statusBusy: boolean;
  /** 上报「维护设置未保存」，用于页签圆点；切页签不再卸载本面板（见 PageTabs.keepMounted）。 */
  onDirtyChange?: (dirty: boolean) => void;
}) {
  const stats = useAdminAppStatsQuery(app.id);
  const trend = stats.data?.trend ?? [];
  const charge7d = trend.reduce((sum, p) => sum + p.charge, 0);
  const title = app.display_name || app.name;

  const save = useSetAppMaintenanceMutation();
  const [enabled, setEnabled] = useState(app.maintenance_mode);
  const [message, setMessage] = useState(app.maintenance_message);
  const [errors, setErrors] = useState<FormErrors>({});

  // 外部刷新（另一个管理员改了 / 本页保存后列表重拉）时同步本地表单。
  // app 由列表行派生（只存 id），保存成功后新值会经由这条 effect 回到表单，
  // 于是"维护模式已开启"的徽标与二次确认都跟着正确。
  useEffect(() => {
    setEnabled(app.maintenance_mode);
    setMessage(app.maintenance_message);
  }, [app.maintenance_mode, app.maintenance_message]);

  // 只在"脏/净"翻转时上报：父组件的回调每次 render 都是新函数。
  const dirty = enabled !== app.maintenance_mode || message.trim() !== app.maintenance_message;
  const reported = useRef(dirty);
  useEffect(() => {
    if (reported.current === dirty) return;
    reported.current = dirty;
    onDirtyChange?.(dirty);
  }, [dirty, onDirtyChange]);

  const online = stats.data?.online;
  const availability = online && online.calls_24h > 0 ? 1 - online.error_rate : 1;

  const submitMaint = async () => {
    // 文案上限后端硬判 200，前端不挡会让运营写完一大段才被拒（且没有字段提示）。
    const next = validateMaintenance(message);
    setErrors(next);
    if (hasError(next)) return;
    if (enabled && !app.maintenance_mode) {
      const ok = await confirmDialog({
        title: "开启维护模式？",
        body:
          "该应用的全部托管密钥调用 /v1 会立即返回 503（约 10 秒内全量生效）。" +
          "支付回调、已提交的异步任务与已生成的产物链接不受影响。",
        okText: "开启维护",
        okVariant: "danger",
      });
      if (!ok) return;
    }
    try {
      await save.mutateAsync({ id: app.id, enabled, message: message.trim() });
      toastOk(enabled ? "维护模式已开启" : "维护设置已保存");
    } catch (e) {
      toastApiError(e, "保存失败");
    }
  };

  const confirmToggle = async () => {
    const disable = app.status === 1;
    const ok = await confirmDialog({
      title: disable ? "停用该应用？" : "启用该应用？",
      body: disable
        ? "停用后不能再为它签发新的托管密钥；已签发的密钥仍可继续调用并继续扣费。要立即断流请开启维护模式。"
        : "启用后用户可以重新为该应用签发托管密钥。",
      okText: disable ? "停用" : "启用",
      okVariant: disable ? "danger" : "primary",
    });
    if (ok) onToggleStatus();
  };

  return (
    <div className="tab-panel">
      {/* ① 应用名片：顶栏没有标题，这里承担"我在看哪个应用"的识别职责。 */}
      <section className="app-hero">
        <EntityIcon url={app.logo_url} name={title} size="md" />
        <div className="app-hero__main">
          <div className="app-hero__name">
            <span className="app-hero__title">{title}</span>
            <Tag tone={app.status === 1 ? "success" : "gray"}>
              {app.status === 1 ? "启用" : "停用"}
            </Tag>
            {app.maintenance_mode ? <Tag tone="warning">维护中</Tag> : null}
            {app.allow_self_issue ? null : <Tag tone="info">仅运营发放</Tag>}
          </div>
          <div className="app-hero__meta">
            <span className="mono">{app.slug}</span>
            <span>
              {app.access_mode === "self_service" ? "用户可自助换取凭证" : "仅管理员发放接入凭证"}
            </span>
            <span>创建于 {formatDateTime(app.created_at)}</span>
          </div>
          {app.description ? <p className="app-hero__desc">{app.description}</p> : null}
        </div>
        <div className="app-hero__acts">
          <button type="button" className="btn" onClick={onOpenKeys}>
            查看密钥
          </button>
          <button type="button" className="btn" onClick={onEdit}>
            编辑基本信息
          </button>
        </div>
      </section>

      {/* ② 四格经营指标：只读列表行，统计接口挂了也照常可读。 */}
      <div className="grid-4">
        {/* "累计消费" 读 api_keys.used：它包含**在途预扣**（冻结额），
            结算后按实际金额回调，所以这个数字不是单调递增的。
            下面的"近 7 日消费合计"读 usages.charge（只算已结算）。
            两者口径不同，必须写明，否则运营会拿它们互相对账并判成"漏扣"。 */}
        <StatCard
          icon="wallet"
          label="累计消费"
          value={<span className="mono">{formatMoney(app.used)}</span>}
          sub="含在途预扣"
        />
        <StatCard
          icon="coin"
          label="已付充值"
          value={<span className="mono">{formatCent(app.paid_cent)}</span>}
          sub={`${app.paid_count} 笔`}
        />
        <StatCard
          icon="key"
          label="托管密钥"
          value={
            <span className="mono">
              {app.active_key_count}
              <span className="muted"> / {app.key_count}</span>
            </span>
          }
          sub="启用 / 全部"
        />
        <StatCard icon="user" label="用户数" value={app.user_count} sub="持有托管密钥" />
      </div>

      {/* ③ 左走势、右当下：两栏而不是上下堆，避免"图表下面还有四张卡"的超长滚动。 */}
      <div className="app-overview-cols">
        <Card title="近 7 日调用" sub="按业务时区分日，无调用的日期补 0">
          {stats.isPending ? (
            <EmptyState variant="loading" title="加载中…" />
          ) : stats.isError || !stats.data ? (
            <EmptyState variant="error" title="统计数据加载失败" desc="请稍后重试" />
          ) : (
            <>
              <TrendChart
                data={trend.map((p) => ({ date: p.date.slice(5), requests: p.requests }))}
                xKey="date"
                series={[{ key: "requests", name: "调用次数" }]}
                height={200}
                valueFormatter={(v) => v.toLocaleString("zh-CN")}
                empty={<EmptyState title="近 7 日没有调用" desc="接入方还没有发起过请求" />}
              />
              <div className="count-line">
                近 7 日消费合计（已结算） <b className="mono">{formatMoney(charge7d)}</b>
              </div>
            </>
          )}
        </Card>

        {/* 运行态：与左图共用同一条 /stats 查询（同一个 queryKey ⇒ 一次请求）。 */}
        <Card
          title="运行态"
          sub="近 24 小时"
          actions={
            online ? <Tag tone={ONLINE_TONE[online.online]}>{ONLINE_TEXT[online.online]}</Tag> : null
          }
        >
          {stats.isPending ? (
            <EmptyState variant="loading" title="加载中…" />
          ) : stats.isError || !online ? (
            // 指标读不出来时维护开关仍然可用 —— 那正是最需要一键断流的时刻。
            <EmptyState variant="error" title="在线指标加载失败" desc="维护开关不受影响" />
          ) : (
            <div className="run-list">
              <RunRow label="调用" value={online.calls_24h.toLocaleString("zh-CN")} />
              <RunRow
                label="错误率"
                value={`${(online.error_rate * 100).toFixed(2)}%`}
                sub={online.errors_24h ? `${online.errors_24h} 次失败` : undefined}
              />
              <RunRow label="消费" value={<span className="mono">{formatMoney(online.charge_24h)}</span>} />
              <RunRow label="可用性" value={`${(availability * 100).toFixed(1)}%`} />
              <RunRow label="最近调用" value={online.last_call_at || "—"} />
            </div>
          )}
        </Card>
      </div>

      {/* ④ 维护模式：只拦存量流量。开关与指标无关，指标失败时更要能一键断流。 */}
      <Card title="维护模式" sub="开启后该应用的托管密钥调用 /v1 返回 503">
        <div className="maintenance-box">
          <SwitchRow
            label="开启维护模式"
            checked={enabled}
            onChange={setEnabled}
            className="switch-row--inline"
            tip="不影响签发新密钥、支付回调与已生成的产物链接"
          />
          <Field
            label="对外提示文案"
            htmlFor="maint-msg"
            hint={errors.message ?? "最多 200 个字符；终端会把它展示给用户"}
            hintWarn={!!errors.message}
            labelExtra={<span className="small muted">{runeLen(message)}/200</span>}
          >
            <Textarea
              id="maint-msg"
              rows={3}
              maxLength={200}
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              placeholder="留空使用默认文案：该应用正在维护中，请稍后重试"
            />
          </Field>
          <div className="form-actions">
            <button
              type="button"
              className="btn btn-primary"
              disabled={save.isPending}
              onClick={() => void submitMaint()}
            >
              {save.isPending ? "保存中…" : "保存维护设置"}
            </button>
          </div>
        </div>
      </Card>

      {/* ⑤ 应用状态（危险操作）：与维护模式互不联动，文案必须写清各自拦什么。 */}
      <Card title="应用状态" sub="停用只拦新签发，已签发的密钥照常可用并继续扣费">
        <div className="app-danger-row">
          <div className="app-danger-row__txt">
            要立即断流（让 /v1 调用返回 503）请开启上面的维护模式；两者互不联动 ——
            一次操作不该同时完成两件事。
          </div>
          <button
            type="button"
            className={app.status === 1 ? "btn btn-danger" : "btn"}
            disabled={statusBusy}
            onClick={() => void confirmToggle()}
          >
            {app.status === 1 ? "停用应用" : "启用应用"}
          </button>
        </div>
      </Card>

      {/* 把「这里配的东西终端怎么读」接到契约页：运营改完品牌/公告/维护文案后，
          最常问的就是"用户那边什么时候能看到、字段叫什么"。 */}
      <div className="count-line">
        终端通过公开接口读取品牌、公告、版本与维护状态：
        <Link to="/admin/api-docs" search={{ ep: "apps.profile" }} className="btn btn-sm btn-text">
          查询应用公开资料
        </Link>
      </div>
    </div>
  );
}
