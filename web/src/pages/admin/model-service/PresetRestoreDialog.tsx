import { useEffect, useState } from "react";

import { Dialog } from "@/components/common/Dialog";
import { EmptyState } from "@/components/common/EmptyState";
import { toastErr, toastOk } from "@/components/common/Toaster";
import { useRestorePresetsMutation } from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import type { PresetRestoreCounts, PresetRestoreGroup } from "@/types";

/**
 * 「恢复出厂」弹窗：**先预览、再确认**，绝不一点就写。
 *
 * 为什么必须有预览这一步：这是全站唯一会覆盖运营改名/改价/改绑定的动作，
 * 而且它按批量生效（默认把所有内置组一起拉回出厂）。没有计数，运营无法判断
 * 「这次会毁掉多少手工调整」，只能凭运气点下去。
 *
 * 预览走后端的 dry-run（同一段代码 + 事务回滚），不是前端自己推算 ——
 * 两套推算逻辑迟早对不上，而"预览说 3 项、实际动了 5 项"比没有预览更糟。
 *
 * 三个默认值都是刻意的：
 *  1. **保留当前启停**（不勾「同时停用」）：恢复配置不该顺手把在跑的渠道停掉；
 *  2. `group` 留空 = 全部内置组（目前 5 组，逐组勾选的复杂度不值当）；
 *  3. 密钥/余额/健康快照永不触碰 —— 文案里必须写出来，否则运营会以为要重填 KEY。
 *
 * 数据来源只有一个：预览看 `preview.data`，确认后看 `apply.data`（服务端真值）。
 * 组件不另存一份 result —— 有了本地副本，"预览说 3、跑了 5"就会被本地副本盖住。
 */
export function PresetRestoreDialog({
  open,
  onClose,
  onDone,
}: {
  open: boolean;
  onClose: () => void;
  /** 真跑成功后的回调（页面据此刷新）；入参是给 toast 的同一句话 */
  onDone?: (summary: string) => void;
}) {
  /** 同时把渠道/模型/绑定恢复为停用（默认关 = 保留当前启停） */
  const [disable, setDisable] = useState(false);
  const preview = useRestorePresetsMutation();
  const apply = useRestorePresetsMutation();

  const runPreview = preview.mutate;

  // 每次打开都重算：上次的计数可能已经过时（运营中途在别的标签页改过配置）。
  useEffect(() => {
    if (!open) return;
    setDisable(false);
    runPreview({ dry_run: true, keep_enabled: true });
  }, [open, runPreview]);

  const shown = apply.data ?? preview.data;
  const err = preview.error ?? apply.error;
  const errText = err instanceof ApiError ? err.message : err ? "请稍后重试" : "";

  const sum = (list: PresetRestoreGroup[]) =>
    list.reduce(
      (acc, g) => ({
        reused: acc.reused + g.channels.reused + g.models.reused + g.bindings.reused,
        created: acc.created + g.channels.created + g.models.created + g.bindings.created,
        skipped: acc.skipped + g.channels.skipped + g.models.skipped + g.bindings.skipped,
        warnings: acc.warnings + (g.warnings?.length ?? 0),
      }),
      { reused: 0, created: 0, skipped: 0, warnings: 0 },
    );

  const totals = sum(shown?.groups ?? []);
  const nothingToDo =
    !!shown && totals.reused === 0 && totals.created === 0 && totals.skipped === 0;

  const confirm = () => {
    apply.mutate(
      { dry_run: false, keep_enabled: !disable, confirm: "RESTORE" },
      {
        onSuccess: (data) => {
          const t = sum(data.groups);
          const summary =
            `已恢复 ${data.groups.length} 组：覆盖 ${t.reused} 项、新建 ${t.created} 项` +
            (t.skipped > 0 ? `，跳过 ${t.skipped} 项` : "");
          // 有跳过项时不能只弹绿色成功：那会让"少恢复了几个"永远查不出来。
          if (t.skipped > 0) toastErr(summary);
          else toastOk(summary);
          onDone?.(summary);
        },
        onError: (e) => {
          toastErr(e instanceof ApiError ? e.message : "恢复出厂失败");
        },
      },
    );
  };

  const cell = (c: PresetRestoreCounts) => (
    <span className="prs-cell">
      <span className={c.reused > 0 ? "prs-n" : "prs-n is-zero"} title="覆盖回出厂值">
        {c.reused}
      </span>
      <span className={c.created > 0 ? "prs-n is-new" : "prs-n is-zero"} title="新建（库里没有）">
        +{c.created}
      </span>
      <span className={c.skipped > 0 ? "prs-n is-skip" : "prs-n is-zero"} title="跳过（自建/冲突）">
        {c.skipped} 跳过
      </span>
    </span>
  );

  const body = () => {
    if (!shown) {
      // 判据用 isError 而不是 isPending：首帧（effect 还没跑）isPending 仍是 false，
      // 按 isPending 判会闪一帧「预览失败」——那是这个弹窗最不该出现的错觉。
      if (!preview.isError) return <EmptyState variant="loading" title="正在统计将被覆盖的内容…" />;
      return (
        <EmptyState
          icon="!"
          title="预览失败"
          desc={errText || "请稍后重试。预览失败时不会写入任何数据。"}
          action={
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => runPreview({ dry_run: true, keep_enabled: !disable })}
            >
              重试
            </button>
          }
        />
      );
    }
    if (shown.groups.length === 0) {
      return (
        <EmptyState
          icon="◇"
          title="没有内置配置"
          desc="这个实例没有可恢复的出厂预设（可能已关闭内置预设）。"
        />
      );
    }
    return (
      <div className="table-wrap">
        <table className="tbl">
          <thead>
            <tr>
              <th>出厂组</th>
              <th>渠道</th>
              <th>模型</th>
              <th>绑定</th>
            </tr>
          </thead>
          <tbody>
            {shown.groups.map((g) => (
              <tr key={g.group}>
                <td data-label="出厂组">
                  <div>{g.label || g.group}</div>
                  <div className="muted small mono">{g.group}</div>
                  {g.warnings?.length ? (
                    <ul className="prs-warn">
                      {g.warnings.map((w) => (
                        <li key={w}>{w}</li>
                      ))}
                    </ul>
                  ) : null}
                </td>
                <td data-label="渠道">{cell(g.channels)}</td>
                <td data-label="模型">{cell(g.models)}</td>
                <td data-label="绑定">{cell(g.bindings)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    );
  };

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="恢复出厂配置"
      size="xl"
      actions={[
        { label: "取消", onClick: onClose },
        {
          label: apply.isPending ? "恢复中…" : "确认恢复",
          variant: "danger",
          onClick: confirm,
          // 预览没成功就不给点：没有计数的确认等于盲点。
          disabled: apply.isPending || !shown || nothingToDo,
        },
      ]}
    >
      <p className="prs-lead">
        把内置渠道与模型拉回出厂值（名称、Base URL、官网、价目、绑定）。
        <b>密钥、余额与健康快照不会被改动</b>，恢复到启用状态也需要你自己点。
      </p>

      {body()}

      {shown && shown.groups.length > 0 ? (
        <div className="prs-sum">
          合计：覆盖 <b>{totals.reused}</b> 项 · 新建 <b>{totals.created}</b> 项
          {totals.skipped > 0 ? ` · 跳过 ${totals.skipped} 项` : ""}
          {apply.data ? "（已生效）" : "（预览，尚未写入）"}
        </div>
      ) : null}

      <label className="prs-opt">
        <input
          type="checkbox"
          checked={disable}
          onChange={(e) => setDisable(e.target.checked)}
          disabled={apply.isPending}
        />
        <span>
          同时把内置渠道、模型与绑定恢复为<b>停用</b>
          <span className="muted small">
            （默认不勾选：保留这些行当前的启停状态，避免正在服务的渠道被顺手停掉）
          </span>
        </span>
      </label>

      {err && shown ? <div className="prs-err">{errText}</div> : null}
    </Dialog>
  );
}
