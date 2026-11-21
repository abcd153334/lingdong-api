import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { Dialog, type DialogAction } from "@/components/common/Dialog";
import { Tag } from "@/components/common/StatusTag";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { ApiError } from "@/lib/apiClient";
import {
  useAdminReleaseQuery,
  useReleaseCancelMutation,
  useReleaseCheckMutation,
  useReleaseCurrentQuery,
  useReleaseUpdateMutation,
} from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { timeAgo } from "@/lib/device";
import type { ReleaseJob, ReleaseManifest } from "@/types";

/** 步骤 → 人话（与 agent 写入的 message 同义，避免同一状态两种说法）。 */
const STEP_LABEL: Record<string, string> = {
  queued: "已提交，等待更新程序领取",
  fetching: "拉取版本信息",
  building: "准备镜像（优先拉预构建，几十秒；否则本地构建 3–8 分钟）",
  backing_up: "备份数据库",
  switching: "切换容器（服务将中断 30–60 秒）",
  verifying: "校验版本与依赖",
  rolling_back: "校验未通过，正在回滚",
  cancelled: "已取消",
  interrupted: "切换中断",
};

const STATUS_LABEL: Record<string, string> = {
  queued: "排队中",
  fetching: "拉取版本",
  building: "构建中",
  backing_up: "备份中",
  switching: "切换中",
  verifying: "校验中",
  rolling_back: "回滚中",
  success: "成功",
  failed: "失败",
  rolled_back: "已回滚",
  manual_required: "需人工处理",
};

/** 失败码 → 下一步做什么。只给机器码等于让管理员自己去翻代码。 */
const FAIL_HINT: Record<string, string> = {
  tag_invalid: "目标版本格式不合法",
  tag_not_found: "远端没有这个 tag，先推 tag 再试",
  commit_mismatch: "tag 指向的提交与清单不一致（tag 可能被移动过）",
  dirty_worktree: "生产目录有未提交改动，先查明来源再更新",
  fetch_failed: "拉取代码失败：检查部署机的仓库凭证与网络",
  checkout_failed: "检出失败，查看 journalctl -u ld-updater",
  preflight_failed: "环境体检未通过，按提示修 .env 或端口",
  build_failed: "构建失败，日志见 journalctl -u ld-updater",
  backup_failed: "备份失败，已中止（无备份不升级）",
  disk_low: "Docker 数据盘空间不足 12GB，先清理旧镜像",
  switch_failed: "新版本启动失败，已回滚",
  verify_failed: "新版本未通过校验，已回滚",
  rollback_unavailable: "回退镜像已被清理，无法自动回滚",
  rollback_failed: "回滚也失败，请立即人工介入",
  agent_offline: "更新程序未运行（未安装或未启动）",
  agent_lost: "更新程序失去联系，本次已中止",
  interrupted_during_switch: "切换容器期间失去联系：服务可能停在新旧之间，请人工确认",
  data_epoch_changed: "期间执行了数据迁移，不能只回退镜像",
};

const MANIFEST_ERROR: Record<string, string> = {
  blocked: "地址被拒绝：必须是 https 公网地址",
  timeout: "抓取超时（目标站点无响应）",
  network: "连不上清单地址（DNS / 证书 / 网络）",
  http_status: "清单地址返回了非 200",
  too_large: "清单内容超过 64KB",
  bad_json: "清单不是合法 JSON，或缺少 latest",
  unsupported_schema: "清单 schema 不是 1",
  invalid_version: "清单里的版本号或 tag 不合法",
  invalid_field: "清单里的 commit 或链接不合法",
};

const BLOCKED_HINT: Record<string, string> = {
  not_configured: "配置发布清单地址后才能更新",
  agent_offline: "更新程序未运行：在部署机执行 sudo bash deploy/agent/install.sh",
  already_running: "已有更新正在进行中",
  manifest_error: "先让清单检查成功",
  version_unparsable: "当前版本不是版本号（本地构建），无法判断先后",
};

function newRequestID(): string {
  const c = globalThis.crypto;
  if (c && typeof c.randomUUID === "function") return c.randomUUID();
  return `req-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

/** 进度条：复用 `.progress` 样式，但不复用 TaskProgress —— 终态集合不同。 */
function Progress({ value }: { value: number }) {
  const p = Math.max(0, Math.min(100, Math.round(value)));
  return (
    <div className="share-cell">
      <div className="progress">
        <i style={{ width: `${p}%` }} />
      </div>
      <span className="num small">{p}%</span>
    </div>
  );
}

function statusTone(status: string): "success" | "warning" | "danger" | "info" {
  if (status === "success") return "success";
  if (status === "failed" || status === "manual_required") return "danger";
  if (status === "rolled_back") return "warning";
  return "info";
}

function JobLine({ job }: { job: ReleaseJob }) {
  return (
    <div className="rel-line">
      <Tag tone={statusTone(job.status)}>{STATUS_LABEL[job.status] ?? job.status}</Tag>
      <span className="small muted mono">{job.target_tag}</span>
      {job.finished_at ? <span className="small muted">· {timeAgo(job.finished_at)}</span> : null}
    </div>
  );
}

/**
 * 版本与更新弹窗。
 *
 * 状态只有一个来源：`GET /admin/system/release`（缓存）与 `GET .../update/current`（进度）。
 * 界面**不自己判断**"能不能更新"：那由后端的 `update.can_update` / `blocked_reason` 决定。
 * 前端重算规则的结果是"按钮能点但请求 409"，而且两边规则迟早会漂。
 */
export function ReleaseDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const release = useAdminReleaseQuery();
  const current = useReleaseCurrentQuery(open);
  const check = useReleaseCheckMutation();
  const update = useReleaseUpdateMutation();
  const cancel = useReleaseCancelMutation();
  const [force, setForce] = useState(false);

  const info = release.data;
  const active = current.data?.active ?? info?.update.active_job ?? null;
  const last = current.data?.last ?? info?.update.last_job ?? null;
  const manifest: ReleaseManifest | undefined = info?.manifest;

  // 打开时自动检查一次：只在"已配置且从未检查过"时触发。
  // 用 ref 而不是依赖 checked_at 的副作用：检查失败时 checked_at 也可能为空，
  // 靠依赖触发会变成对目标站点的连打。
  const autoTried = useRef(false);
  useEffect(() => {
    if (!open) {
      autoTried.current = false;
      setForce(false);
      return;
    }
    if (autoTried.current) return;
    if (!manifest?.configured || manifest.checked_at) return;
    autoTried.current = true;
    check.mutate();
  }, [open, manifest?.configured, manifest?.checked_at, check]);

  const targetTag = manifest?.latest?.tag ?? "";
  const blocked = info?.update.blocked_reason ?? "";
  const canUpdate = Boolean(info?.update.can_update) && !active;

  const onUpdate = async () => {
    if (!targetTag) return;
    const running = info?.running_tasks ?? 0;
    const ok = await confirmDialog({
      title: `更新到 ${targetTag}？`,
      body: (
        <div className="small">
          <p>将依次执行：拉取代码 → 环境体检 → 准备镜像（优先拉预构建，几十秒）→ 备份数据库 → 切换容器。</p>
          <p>
            <b>切换瞬间管理台与 API 会中断约 30–60 秒</b>，正在进行的同步请求可能失败。
            {running > 0 ? ` 当前有 ${running} 个任务在排队或运行，异步任务不会因此丢失。` : ""}
          </p>
          <p>新版本未通过校验会自动回滚到当前版本。</p>
        </div>
      ),
      okText: "开始更新",
      okVariant: "danger",
    });
    if (!ok) return;
    update.mutate(
      { target_tag: targetTag, request_id: newRequestID(), force },
      {
        onSuccess: () => toast.success("已提交，正在等待更新程序领取"),
        onError: (err) => {
          const e = err as ApiError;
          // 被"同版本刚失败过 / 降级"挡下时，给一次显式强制提交的机会：
          // 规则由后端给，人要有最终决定权，否则一条写错的清单就让人无路可走。
          if (e?.errorCode === "release_update_blocked") setForce(true);
          toast.error(e?.message || "提交失败");
        },
      },
    );
  };

  const onCancel = async (job: ReleaseJob) => {
    const ok = await confirmDialog({
      title: "取消这次更新？",
      body: <div className="small">只有尚未开始执行的更新可以取消；已进入构建或切换的不能取消。</div>,
      okText: "取消更新",
      okVariant: "danger",
      cancelText: "返回",
    });
    if (!ok) return;
    cancel.mutate(job.id, {
      onSuccess: () => toast.success("已取消"),
      onError: (err) => toast.error((err as ApiError)?.message || "取消失败"),
    });
  };

  const justSucceeded = !active && last?.status === "success";
  const actions: DialogAction[] = [{ label: "关闭", onClick: onClose }];
  if (active) {
    if (active.status === "queued") actions.push({ label: "取消更新", onClick: () => void onCancel(active) });
  } else if (justSucceeded) {
    // 新版本的前端 chunk 与 index.html 都变了：必须整页重载，否则会用到已删除的旧 chunk。
    actions.push({ label: "刷新页面", variant: "primary", onClick: () => window.location.reload() });
  } else {
    if (manifest?.update_available) {
      actions.push({
        label: force ? "强制更新" : "立即更新",
        variant: "primary",
        disabled: !canUpdate || update.isPending,
        onClick: () => void onUpdate(),
      });
    }
    actions.push({
      label: check.isPending ? "检查中…" : "重新检查",
      disabled: check.isPending,
      onClick: () => check.mutate(),
    });
  }

  return (
    <Dialog open={open} onClose={onClose} title="版本与更新" size="lg" actions={actions}>
      <div className="rel-body">
        <div className="cd-kv">
          <div className="cd-kv-row">
            <span className="k">当前版本</span>
            <span className="v mono">{info?.current.version ?? "—"}</span>
          </div>
          <div className="cd-kv-row">
            <span className="k">更新程序</span>
            <span className="v">
              {info?.agent.online ? (
                <>
                  <Tag tone="success">在线</Tag>
                  <span className="small muted" style={{ marginLeft: 8 }}>
                    {info.agent.host}
                    {info.agent.current_tag ? ` · ${info.agent.current_tag}` : ""}
                  </span>
                </>
              ) : (
                <Tag tone="warning">未运行</Tag>
              )}
            </span>
          </div>
          {manifest?.checked_at ? (
            <div className="cd-kv-row">
              <span className="k">上次检查</span>
              <span className="v small muted">{formatDateTime(manifest.checked_at)}</span>
            </div>
          ) : null}
        </div>

        {manifest && !manifest.configured ? (
          <div className="small muted">
            还没有配置发布清单地址。到「系统配置 → 通用 → 版本与更新」填一个 https 地址，系统就能比对最新版本。
          </div>
        ) : null}

        {manifest?.configured && manifest.error_kind && !active ? (
          <div className="rel-err">
            无法确认最新版本：{MANIFEST_ERROR[manifest.error_kind] ?? manifest.error_kind}
            {manifest.http_status ? `（HTTP ${manifest.http_status}）` : ""}
          </div>
        ) : null}

        {manifest?.configured && !manifest.error_kind && manifest.latest ? (
          <div className="rel-sec">
            {manifest.update_available ? (
              <>
                <div className="rel-line">
                  <Tag tone="warning">有新版本</Tag>
                  <span className="mono">{manifest.latest.version}</span>
                  {manifest.latest.published_at ? (
                    <span className="small muted">{manifest.latest.published_at}</span>
                  ) : null}
                </div>
                {manifest.latest.notes ? <div className="small">{manifest.latest.notes}</div> : null}
                {manifest.latest.commit ? (
                  <div className="small muted mono">提交 {manifest.latest.commit.slice(0, 12)}</div>
                ) : null}
                {manifest.latest.url ? (
                  <a className="small" href={manifest.latest.url} target="_blank" rel="noreferrer">
                    查看发布说明
                  </a>
                ) : null}
              </>
            ) : (
              <div className="rel-line">
                <Tag tone="success">已是最新</Tag>
                <span className="small muted mono">{manifest.latest.version}</span>
                {!manifest.comparable ? (
                  <span className="small muted">（当前版本不是版本号，无法比较：本地构建的镜像）</span>
                ) : null}
              </div>
            )}
          </div>
        ) : null}

        {active ? (
          <div className="rel-sec">
            <JobLine job={active} />
            <Progress value={active.progress} />
            <div className="small muted">{STEP_LABEL[active.step] ?? active.message}</div>
            {active.status === "switching" || active.status === "verifying" ? (
              <div className="small muted">这段时间接口可能短暂不可用；页面会自动重连，不需要手动刷新。</div>
            ) : null}
          </div>
        ) : null}

        {!active && last ? (
          <div className="rel-sec">
            <JobLine job={last} />
            <div className="small muted">{last.message}</div>
            {last.failure_code ? (
              <div className="rel-err">{FAIL_HINT[last.failure_code] ?? last.failure_code}</div>
            ) : null}
            {last.rollback_result === "ok" ? (
              <div className="small muted">已自动回滚，当前运行的仍是更新前的版本。</div>
            ) : null}
          </div>
        ) : null}

        {!active && !canUpdate && blocked && blocked !== "no_update" ? (
          <div className="small muted">{BLOCKED_HINT[blocked] ?? ""}</div>
        ) : null}

        {force ? (
          <div className="rel-warn">
            上次提交被拒（可能是同一版本刚失败过，或目标版本低于当前版本）。确认要继续时点「强制更新」。
          </div>
        ) : null}

        <div className="small muted">
          更新由部署机上的独立程序执行，后端不会获得 Docker 权限；任务与进度保存在数据库里，
          关掉页面或服务重启都不会丢。
        </div>
      </div>
    </Dialog>
  );
}
