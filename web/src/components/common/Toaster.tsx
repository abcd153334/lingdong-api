import { Toaster as SonnerToaster, toast } from "sonner";
import { useThemeStore } from "@/stores/theme";

/** 全局提示（sonner 承载，替代原型 #toast；顶部居中与原型视觉位一致） */
export function Toaster() {
  const theme = useThemeStore((s) => s.theme);
  return (
    <SonnerToaster
      theme={theme}
      position="top-center"
      richColors
      closeButton
      toastOptions={{ style: { fontSize: "13px", borderRadius: "12px" } }}
    />
  );
}

export const toastOk = (message: string) => toast.success(message);
export const toastErr = (message: string) => toast.error(message);
export const toastInfo = (message: string) => toast(message);
