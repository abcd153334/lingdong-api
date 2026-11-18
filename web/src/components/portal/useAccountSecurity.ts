import { useEffect, useState } from "react";
import { useChannelAvailability } from "@/lib/contactChannel";
import type { ContactChannel, MeInfo } from "@/types";

/** 账户安全区的交互状态（抽屉开合 + 通道可用性覆盖）。 */
export interface AccountSecurityState {
  email: string;
  phone: string;
  /** 通道是否可用（配置就绪 + 运行时未被后端否决） */
  availableOf: (ch: ContactChannel) => boolean;
  passOpen: boolean;
  setPassOpen: (open: boolean) => void;
  bindChannel: ContactChannel | null;
  setBindChannel: (ch: ContactChannel | null) => void;
  /** 后端返回「该通道未启用」时的本地降级 */
  onUnavailable: (ch: ContactChannel) => void;
}

/**
 * 账户安全区的状态唯一实现（唯一消费方：`AccountDialog` 的「账户安全」分区）。
 *
 * 为什么单独成 hook：抽屉状态若散在弹窗与落地页两处 `useState(passOpen/bindChannel)`，
 * 结局是「改完密码抽屉状态对不上」这类无法复现的问题。抽屉本身仍由宿主渲染
 * （必须在弹层的**兄弟位置**，不能进 `<Dialog>` 内部）。
 */
export function useAccountSecurity(opts: { me?: MeInfo; mustChange: boolean }): AccountSecurityState {
  const { me, mustChange } = opts;
  const email = me?.email ?? "";
  const phone = me?.phone ?? "";

  /* 绑定通道可用性：判据是**传输就绪**（与服务端 BindContact 一致，与注册渠道策略无关）。
     后端拒绝时用本地覆盖降级，防止「配置说可用、实际发不出去」。 */
  const { isAvailable } = useChannelAvailability();
  const [overrides, setOverrides] = useState<Partial<Record<ContactChannel, boolean>>>({});

  const [passOpen, setPassOpen] = useState(false);
  // 临时口令账号的唯一可用操作就是改密：直接把抽屉打开，省掉一次「为什么点不动」。
  useEffect(() => {
    if (mustChange) setPassOpen(true);
  }, [mustChange]);

  // bindChannel 非空 = 抽屉打开（不额外维护 bindOpen，避免两个状态打架）
  const [bindChannel, setBindChannel] = useState<ContactChannel | null>(null);

  return {
    email,
    phone,
    availableOf: (ch) => overrides[ch] ?? isAvailable(ch),
    passOpen,
    setPassOpen,
    bindChannel,
    setBindChannel,
    onUnavailable: (ch) => setOverrides((prev) => ({ ...prev, [ch]: false })),
  };
}
