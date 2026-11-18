import { useState } from "react";
import { Dialog } from "@/components/common/Dialog";
import { EntityIcon } from "@/components/common/EntityIcon";
import { Icon } from "@/components/common/Icon";
import { ModelTagList } from "@/components/common/ModelTag";
import { ApiError } from "@/lib/apiClient";
import type { AgentChatModel } from "@/lib/api/agent";
import { useRenameAgentSessionMutation } from "@/lib/api/agent";
import { cn } from "@/lib/utils";

export interface AgentModelDialogProps {
  open: boolean;
  onClose: () => void;
  sessionId: number;
  models: AgentChatModel[];
  /** 会话当前指定的模型（空串 = 跟随全站默认） */
  current: string;
  /** 全站默认（设置未指定时由系统派生） */
  effective: string;
}

/**
 * 选择本会话使用的对话模型。
 *
 * 存在两个层级，必须分清：
 *  - 这里选的是**会话级**（写 `agent_sessions.model_id`，刷新/换设备都还在）；
 *  - 设置弹窗里的是**全站默认**（所有会话的兜底）。
 * 把两者合成一个开关，就会出现"我明明换了模型，别人也被换了"。
 */
export function AgentModelDialog({
  open,
  onClose,
  sessionId,
  models,
  current,
  effective,
}: AgentModelDialogProps) {
  const save = useRenameAgentSessionMutation();
  const [err, setErr] = useState("");

  const pick = (id: string) => {
    setErr("");
    save.mutate(
      { id: sessionId, model_id: id },
      {
        onSuccess: onClose,
        onError: (e) => setErr(e instanceof ApiError ? e.message : "切换失败，请重试"),
      },
    );
  };

  return (
    <Dialog open={open} onClose={onClose} title="对话模型" size="md" portal layer={2}>
      <div className="agent-dlg" data-testid="agent-model-dialog">
        <p className="agent-dlg__lead">
          只影响这个会话。全站默认在「运行参数」里改。
        </p>
        <ul className="agent-picklist">
          <li>
            <button
              type="button"
              className={cn("agent-pick", !current && "is-on")}
              onClick={() => pick("")}
              disabled={save.isPending}
            >
              <span className="agent-pick__main">
                <span className="agent-pick__name">跟随全站默认</span>
                <span className="agent-pick__sub">{effective || "当前没有可用模型"}</span>
              </span>
              {!current ? <Icon name="check" size={15} /> : null}
            </button>
          </li>
          {models.map((m) => (
            <li key={m.id}>
              <button
                type="button"
                className={cn("agent-pick", current === m.id && "is-on")}
                onClick={() => pick(m.id)}
                disabled={save.isPending}
              >
                <EntityIcon url={m.icon_url ?? ""} name={m.display_name || m.id} color={m.vendor_color} size="sm" />
                <span className="agent-pick__main">
                  <span className="agent-pick__name">{m.display_name || m.id}</span>
                  <span className="agent-pick__sub">
                    {m.vendor_name ? `${m.vendor_name} · ` : ""}
                    {m.id}
                  </span>
                  {/* 行高固定：最多 2 个，其余折成 +N */}
                  <ModelTagList tags={m.tags} max={2} />
                </span>
                {current === m.id ? <Icon name="check" size={15} /> : null}
              </button>
            </li>
          ))}
        </ul>
        {models.length === 0 ? (
          <div className="agent-alert agent-alert--warn">
            还没有可用的对话模型，请先在模型管理里配置并绑定渠道
          </div>
        ) : null}
        {err ? <div className="agent-alert agent-alert--bad">{err}</div> : null}
      </div>
    </Dialog>
  );
}
