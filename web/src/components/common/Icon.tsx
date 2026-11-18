import {
  Activity,
  AlertTriangle,
  ArrowLeft,
  ArrowUp,
  Bell,
  BookOpen,
  Brain,
  Check,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  ChevronUp,
  CircleDollarSign,
  Clock,
  Copy,
  CreditCard,
  Download,
  ExternalLink,
  Eye,
  EyeOff,
  FileText,
  Filter,
  HelpCircle,
  Home,
  Key,
  LayoutGrid,
  Library,
  Link2,
  List,
  LogOut,
  Menu,
  MessageSquarePlus,
  Monitor,
  Moon,
  MoreHorizontal,
  Paperclip,
  Pencil,
  Pin,
  Plus,
  Power,
  RefreshCw,
  Search,
  Settings,
  Shield,
  Sparkles,
  Split,
  Square,
  Sun,
  Trash2,
  Undo2,
  Upload,
  User,
  Users,
  Wallet,
  Wrench,
  X,
  Zap,
} from "lucide-react";
import type { LucideIcon } from "lucide-react";

/**
 * 图标映射（对齐 prototype.js IC 图标表，语义一对一）
 * 统一 stroke 1.5，与原型图标视觉一致。
 */
export const ICON_MAP = {
  arrowLeft: ArrowLeft,
  home: Home,
  user: User,
  grid: LayoutGrid,
  key: Key,
  rows: List,
  wallet: Wallet,
  coin: CircleDollarSign,
  gear: Settings,
  shield: Shield,
  search: Search,
  plus: Plus,
  copy: Copy,
  eye: Eye,
  close: X,
  menu: Menu,
  moon: Moon,
  sun: Sun,
  refresh: RefreshCw,
  external: ExternalLink,
  logout: LogOut,
  zap: Zap,
  check: Check,
  help: HelpCircle,
  doc: FileText,
  book: BookOpen,
  bell: Bell,
  filter: Filter,
  chevronLeft: ChevronLeft,
  chevronRight: ChevronRight,
  chevronDown: ChevronDown,
  chevronUp: ChevronUp,
  download: Download,
  /* 管理台菜单分组与台账用的语义图标 */
  users: Users,
  creditCard: CreditCard,
  activity: Activity,
  library: Library,
  /* 对话工作台用的图标（AI 接入助手） */
  send: ArrowUp,
  stop: Square,
  attach: Paperclip,
  spark: Sparkles,
  brain: Brain,
  edit: Pencil,
  wrench: Wrench,
  trash: Trash2,
  more: MoreHorizontal,
  message: MessageSquarePlus,
  clock: Clock,
  alert: AlertTriangle,
  /* 账户设置分区导航：设备与会话用屏幕图标（lucide 没有「登录设备」这个语义） */
  monitor: Monitor,
  /* 行内动作（表格操作列）：语义必须一对一，缺字形先在这里补，
     不要在页面里拿 zap/check 之类的近似图标硬凑——那样同一动作两种图标。 */
  power: Power,
  link: Link2,
  pin: Pin,
  split: Split,
  eyeOff: EyeOff,
  upload: Upload,
  undo: Undo2,
} as const;

export type IconName = keyof typeof ICON_MAP;

export interface IconProps {
  name: IconName;
  size?: number;
  className?: string;
}

export function Icon({ name, size = 18, className }: IconProps) {
  const Cmp: LucideIcon = ICON_MAP[name];
  return <Cmp size={size} strokeWidth={1.5} className={className} aria-hidden="true" />;
}
