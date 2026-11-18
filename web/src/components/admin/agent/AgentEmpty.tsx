import { Icon, type IconName } from "@/components/common/Icon";

interface Guide {
  icon: IconName;
  title: string;
  desc: string;
  /** 填入输入框的文案（不直接发送：先让人改字） */
  fill: string;
  /** 需要先打开素材弹窗（贴文档是这类需求的第一步） */
  docs?: boolean;
}

const GUIDES: Guide[] = [
  {
    icon: "zap",
    title: "接入一家新供应商",
    desc: "贴文档或直接描述，助手会归纳端点、计价格与限额",
    fill: "接入一家新的视频供应商，文档我贴在下面，先把规格和渠道建出来",
    docs: true,
  },
  {
    icon: "coin",
    title: "给现有模型改价",
    desc: "按档位说明新价格，助手会算出额度并生成变更提案",
    fill: "把现有的视频模型 720p 档价格改成 0.35 元每秒",
  },
  {
    icon: "alert",
    title: "排查渠道失败",
    desc: "让助手核对密钥、地址与适配器，必要时做一次零成本探测",
    fill: "有个渠道一直报 401，帮我确认是密钥失效还是地址配错了",
  },
  {
    icon: "shield",
    title: "能不能声明式接入",
    desc: "先判断该上游能不能用规格接入，不能就说缺什么",
    fill: "这家上游能不能用声明式规格接入？如果不能，告诉我缺哪些能力",
  },
];

/**
 * 空态：一句说明 + 四张引导卡。
 *
 * 为什么是"填入输入框"而不是"点一下直接发"：这些是模板，每家厂商的差别
 * 就在那一两个词上（模型名、档位、价格）。直接发送会立刻烧掉一轮编排成本，
 * 而结果大概率还得重来。
 */
export function AgentEmpty({
  onPick,
  onOpenDocs,
}: {
  onPick: (text: string) => void;
  onOpenDocs: () => void;
}) {
  return (
    <div className="agent-empty">
      <div className="agent-empty__hero">
        <span className="agent-empty__mark">
          <Icon name="spark" size={20} />
        </span>
        <h2>今天要接什么？</h2>
        <p>我能读文档、查平台现状、做连通性探测，然后把改动整理成提案，由你确认后才写入。</p>
      </div>
      <div className="agent-empty__grid">
        {GUIDES.map((g) => (
          <button
            key={g.title}
            type="button"
            className="agent-guide"
            onClick={() => {
              onPick(g.fill);
              if (g.docs) onOpenDocs();
            }}
          >
            <span className="agent-guide__icon">
              <Icon name={g.icon} size={15} />
            </span>
            <span className="agent-guide__title">{g.title}</span>
            <span className="agent-guide__desc">{g.desc}</span>
          </button>
        ))}
      </div>
    </div>
  );
}
