import { useEffect, useRef, useState } from "react";
import { Link } from "@tanstack/react-router";

import { Card } from "@/components/common/Card";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { Field } from "@/components/common/Field";
import { EmptyState } from "@/components/common/EmptyState";
import { StatCard } from "@/components/common/StatCard";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Tag } from "@/components/common/StatusTag";
import { Textarea } from "@/components/common/Textarea";
import { toastOk } from "@/components/common/Toaster";
import { toastApiError } from "@/lib/apiToast";
import { useAdminAppStatsQuery, useSetAppMaintenanceMutation } from "@/lib/api";
import { formatMoney } from "@/lib/format";
import type { AdminAppItem } from "@/types";

import { hasError, runeLen, validateMaintenance, type FormErrors } from "../validation";

const ONLINE_TEXT = { online: "在线", idle: "空闲", offline: "离线" } as const;

/**
 * 在线（运行态 + 维护模式）。
 *
 * 维护模式与「停用」严格分工：
 *   - 停用（概览页签）只拦**新签发**，存量密钥照跑；
 *   - 维护只拦**存量流量**（托管密钥调 /v1 返回 503），不影响签发。
 *
 * 开启前必须二次确认，并把"会拦什么、不会拦什么"写清楚：这是一次
 * 面向全量终端的中断，不是普通的开关。
 *
 * 数据来源：运行态走 `/stats`（与概览共用），维护态走列表行 ——
 * **维护开关不受指标加载失败影响**：指标读不出来时更要能一键关掉维护。
 */
export function RuntimeTab({
  app,
  onDirtyChange,
}: {
  app: AdminAppItem;
  /** 上报「有未保存修改」，用于页签圆点；切页签不再卸载本面板（见 PageTabs.keepMounted）。 */
  onDirtyChange?: (dirty: boolean) => void;
}) {
  const stats = useAdminAppStatsQuery(app.id);
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

  const submit = async () => {
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

  return (
    <div className="tab-panel">
      <div className="app-status-hero">
        <span className={`dot ${online?.online ?? "offline"}`} aria-hidden="true" />
        <div className="main">
          <div className="title">{online ? ONLINE_TEXT[online.online] : "读取中…"}</div>
          <div className="small muted">
            {online
              ? `最近调用 ${online.last_call_at || "—"} · 24h 可用性 ${(availability * 100).toFixed(1)}%`
              : "正在读取在线指标"}
          </div>
        </div>
        {app.maintenance_mode ? <Tag tone="warning">维护模式已开启</Tag> : null}
      </div>

      {stats.isPending ? (
        <EmptyState variant="loading" title="加载中…" />
      ) : stats.isError || !online ? (
        <EmptyState variant="error" title="在线指标加载失败" desc="维护开关仍可使用" />
      ) : (
        <div className="grid-4">
          {/* 活跃密钥与概览四格里的「托管密钥」是同一个数，取列表行那一份。 */}
          <StatCard icon="key" label="活跃密钥" value={app.active_key_count} sub="状态为启用" />
          <StatCard
            icon="zap"
            label="24h 调用"
            value={<span className="mono">{online.calls_24h.toLocaleString("zh-CN")}</span>}
          />
          <StatCard
            icon="shield"
            label="24h 错误率"
            value={`${(online.error_rate * 100).toFixed(2)}%`}
            sub={`${online.errors_24h} 次失败`}
          />
          <StatCard
            icon="wallet"
            label="24h 消费"
            value={<span className="mono">{formatMoney(online.charge_24h)}</span>}
          />
        </div>
      )}

      <Card title="维护模式" sub="开启后该应用的托管密钥调用 /v1 返回 503">
        <div className="maintenance-box">
          <SwitchRow
            label="开启维护模式"
            checked={enabled}
            onChange={setEnabled}
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
            <button type="button" className="btn btn-primary" disabled={save.isPending} onClick={() => void submit()}>
              {save.isPending ? "保存中…" : "保存维护设置"}
            </button>
          </div>
        </div>
      </Card>

      <div className="alert-banner warn">
        <span>
          维护模式只影响 /v1 调用；要阻止新接入请到「概览」停用应用。两者互不联动 ——
          一次操作不该同时完成两件事。
        </span>
      </div>

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
