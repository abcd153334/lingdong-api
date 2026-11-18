import { useNavigate } from "@tanstack/react-router";
import { useForm, type SubmitErrorHandler } from "react-hook-form";
import { toast } from "sonner";
import { PasswordField } from "@/components/auth/AuthFields";
import { Drawer } from "@/components/common/Drawer";
import { useChangePasswordMutation } from "@/lib/api";
import { passwordStrength, zodResolver } from "@/lib/form";
import { changePasswordSchema, type ChangePasswordForm } from "@/lib/schemas";
import { useAuthStore } from "@/stores/auth";

export interface ChangePasswordDrawerProps {
  open: boolean;
  onClose: () => void;
}

/**
 * 修改密码抽屉（用户中心与管理台「账户设置」共用，唯一实现）。
 *
 * 成功后**必须**清本地会话并跳登录页：后端改密会撤销全部会话
 * （`auth.setPassword`），留在原页面只会得到一片 401。
 */
export function ChangePasswordDrawer({ open, onClose }: ChangePasswordDrawerProps) {
  const navigate = useNavigate();
  const clearAuth = useAuthStore((s) => s.clear);
  const changePassword = useChangePasswordMutation();

  const form = useForm<ChangePasswordForm>({
    resolver: zodResolver<ChangePasswordForm>(changePasswordSchema),
    defaultValues: { old_password: "", new_password: "", confirm_password: "" },
  });
  const newPass = form.watch("new_password");

  const submit = (values: ChangePasswordForm) => {
    changePassword.mutate(
      { old_password: values.old_password, new_password: values.new_password },
      {
        onSuccess: () => {
          toast.success("密码已修改，所有设备需重新登录");
          clearAuth();
          void navigate({ to: "/login" });
        },
      },
    );
  };
  // 校验失败的根文案取自 RHF 传入的 errors（formState 此时尚未同步）
  const onInvalid: SubmitErrorHandler<ChangePasswordForm> = (errors) => {
    const message = errors.root?.message;
    if (message) toast.error(message);
  };

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title="修改密码"
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            取消
          </button>
          <button
            type="button"
            className="btn btn-primary"
            onClick={form.handleSubmit(submit, onInvalid)}
          >
            修改密码
          </button>
        </>
      }
    >
      <div className="alert-banner warn">
        修改密码后，所有已登录的设备会立即退出，需要重新登录。
      </div>
      <form onSubmit={form.handleSubmit(submit, onInvalid)}>
        <PasswordField
          id="pw-old"
          label="原密码"
          autoComplete="current-password"
          registration={form.register("old_password")}
        />
        <PasswordField
          id="pw-new"
          label="新密码"
          autoComplete="new-password"
          registration={form.register("new_password")}
          strength={passwordStrength(newPass)}
        />
        <PasswordField
          id="pw-new2"
          label="确认新密码"
          autoComplete="new-password"
          registration={form.register("confirm_password")}
        />
      </form>
    </Drawer>
  );
}
