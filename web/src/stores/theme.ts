import { create } from "zustand";
import type { ThemeMode } from "@/types";

/** 主题持久化键（冻结契约：ld-theme） */
export const THEME_KEY = "ld-theme";

/** 主题唯一写入口（对齐 prototype.js applyTheme）：写入 <html data-theme> + localStorage */
export function applyTheme(theme: ThemeMode): void {
  if (typeof document !== "undefined") {
    document.documentElement.setAttribute("data-theme", theme);
  }
  try {
    localStorage.setItem(THEME_KEY, theme);
  } catch {
    /* 隐私模式等 localStorage 不可用场景：降级为内存态，不报错 */
  }
}

function readInitialTheme(): ThemeMode {
  if (typeof document !== "undefined") {
    const attr = document.documentElement.getAttribute("data-theme");
    if (attr === "light" || attr === "dark") return attr;
  }
  return "light";
}

interface ThemeState {
  theme: ThemeMode;
  setTheme: (theme: ThemeMode) => void;
  toggle: () => void;
}

/** 主题态：默认 light；applyTheme 为唯一写入口 */
export const useThemeStore = create<ThemeState>((set, get) => ({
  theme: readInitialTheme(),
  setTheme: (theme) => {
    applyTheme(theme);
    set({ theme });
  },
  toggle: () => get().setTheme(get().theme === "dark" ? "light" : "dark"),
}));
