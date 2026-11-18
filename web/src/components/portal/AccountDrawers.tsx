import type { ContactChannel } from "@/types";
import { BindContactDrawer } from "./BindContactDrawer";
import { ChangePasswordDrawer } from "./ChangePasswordDrawer";

export interface AccountDrawersProps {
  passOpen: boolean;
  onClosePass: () => void;
  bindChannel: ContactChannel | null;
  onCloseBind: () => void;
  email: string;
  phone: string;
  onUnavailable: (ch: ContactChannel) => void;
}

/**
 * 改密 / 换绑两个抽屉的唯一组装点。
 *
 * **必须由宿主渲染在弹层的兄弟位置，不能放进 `<Dialog>` 内部**：`.dialog` 桌面有
 * `transform`、移动端有 `translate`（都建立 fixed 包含块），`.dl-body` 又是
 * `overflow-y:auto` —— 抽屉会被钉在弹窗盒子里并被裁掉。
 */
export function AccountDrawers({
  passOpen,
  onClosePass,
  bindChannel,
  onCloseBind,
  email,
  phone,
  onUnavailable,
}: AccountDrawersProps) {
  return (
    <>
      <ChangePasswordDrawer open={passOpen} onClose={onClosePass} />
      {bindChannel && (
        <BindContactDrawer
          key={bindChannel}
          channel={bindChannel}
          email={email}
          phone={phone}
          onClose={onCloseBind}
          onUnavailable={onUnavailable}
        />
      )}
    </>
  );
}
