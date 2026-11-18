import { useSyncExternalStore, type ReactNode } from "react";
import { Dialog } from "./Dialog";
import type { BtnVariant } from "./PageHead";

export interface ConfirmOptions {
  title?: string;
  body?: ReactNode;
  okText?: string;
  cancelText?: string;
  /** 确认按钮变体，默认 primary；危险操作传 "danger" */
  okVariant?: BtnVariant;
  wide?: boolean;
}

interface PendingConfirm {
  opts: ConfirmOptions;
  resolve: (ok: boolean) => void;
}

let pending: PendingConfirm | null = null;
const listeners = new Set<() => void>();

function emit(): void {
  listeners.forEach((l) => l());
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function getSnapshot(): PendingConfirm | null {
  return pending;
}

/**
 * 确认对话框（Promise 化）
 * 契约：重复调用时前一个以「取消」结算（false），避免多实例叠加导致悬挂 Promise。
 */
export function confirmDialog(opts: ConfirmOptions = {}): Promise<boolean> {
  return new Promise<boolean>((resolve) => {
    if (pending) {
      const prev = pending;
      pending = null;
      prev.resolve(false);
    }
    pending = { opts, resolve };
    emit();
  });
}

function settle(ok: boolean): void {
  const current = pending;
  pending = null;
  emit();
  current?.resolve(ok);
}

/** 确认对话框宿主（挂载于应用根部一次即可；Esc/焦点环/滚动锁由 Dialog 统一承担） */
export function ConfirmDialogHost() {
  const current = useSyncExternalStore(subscribe, getSnapshot, getSnapshot);

  if (!current) return null;
  const { opts } = current;
  return (
    <Dialog
      open
      onClose={() => settle(false)}
      title={opts.title ?? "确认操作"}
      size={opts.wide ? "lg" : "md"}
      actions={[
        { label: opts.cancelText ?? "取消", onClick: () => settle(false) },
        {
          label: opts.okText ?? "确认",
          variant: opts.okVariant ?? "primary",
          onClick: () => settle(true),
        },
      ]}
    >
      <div className="confirm-body">{opts.body}</div>
    </Dialog>
  );
}
