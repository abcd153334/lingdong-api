import { useThemeStore } from "@/stores/theme";
import { Icon } from "@/components/common/Icon";

/** 明暗主题切换（唯一作者：stores/theme.applyTheme） */
export function ThemeToggle() {
  const theme = useThemeStore((s) => s.theme);
  const toggle = useThemeStore((s) => s.toggle);
  const dark = theme === "dark";
  return (
    <button
      type="button"
      className="icon-btn"
      aria-label={dark ? "切换到浅色主题" : "切换到深色主题"}
      title={dark ? "浅色模式" : "深色模式"}
      onClick={toggle}
    >
      <Icon name={dark ? "sun" : "moon"} size={18} />
    </button>
  );
}
