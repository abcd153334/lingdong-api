import { useShallow } from "zustand/react/shallow";
import { useThemeStore } from "@/stores/theme";

/** 主题 hook（S0 占位，S1 由 chrome 组件消费） */
export function useTheme() {
  return useThemeStore(
    useShallow((s) => ({ theme: s.theme, setTheme: s.setTheme, toggle: s.toggle })),
  );
}
