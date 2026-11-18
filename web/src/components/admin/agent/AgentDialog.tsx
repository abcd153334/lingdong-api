import { Dialog } from "@/components/common/Dialog";
import { AgentWorkspace } from "./AgentWorkspace";

/**
 * AI 接入助手**弹窗**形态（入口：「新建适配器」）。
 *
 * 与 `/admin/agent` 页共用同一个 `AgentWorkspace`，三处差异都是必须的：
 *  1. `heading={false}`：宿主页面已有 h1，两个 level-1 会撞 layout 门禁的
 *     `getByRole("heading",{level:1})`；
 *  2. `portal`：工作台内部还有设置/文档/模型三个 `<Dialog>`，本弹窗的 `.dialog`
 *     带 `transform`、是 fixed 后代的包含块，不升到 body 会被裁在弹窗盒子里；
 *  3. `scope={["adapter_spec"]}`：这个入口只允许应用「声明式规格」类变更，
 *     越界由后端 apply 拒绝（`agent_proposal_out_of_scope`）。
 *
 * 宿主负责懒挂载与开关；**关闭时不做任何中止**：正在跑的一轮属于会话，
 * 关掉弹窗再打开应该还能看到它继续（终止由「切会话/新建会话」负责）。
 */
export function AgentDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  return (
    <Dialog
      open={open}
      onClose={onClose}
      size="xl"
      closeButton
      portal
      className="agent-dialog"
      title="新建适配器"
    >
      <AgentWorkspace heading={false} scope={["adapter_spec"]} />
    </Dialog>
  );
}
