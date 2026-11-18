package billing

import "lingdong-api/internal/model"

// 图片「冻结张数」的唯一实现。
//
// 为什么放在 billing 而不是 relay：同一条规则有两个入口 —— 同步网关（relay）与
// 异步任务（task.Submit）。以前只有 relay 有一份，异步链路只能靠 400 拦住
// 「组图 / 图层拆分」，因为提交期算不出冻结张数。规则下沉到这里之后，
// 两边调同一个函数，异步也可以按 max_n 冻结、按实际产物结算。
//
// 这里只回答「这一次最多可能产出几张」（冻结用）；「实际交付几张」由
// ResolveImageCount / applyArtifactActuals 负责，两者不混。

// ImageCountInput 是一次图片请求里与张数有关的所有字段。
//
// 用结构体而不是位置参数：`n` 与 `maxImages` 都是一串整数，写反了编译期看不出来，
// 后果是「按 1 张冻结、按 15 张结算」。
type ImageCountInput struct {
	// N 是请求里的 n（<=0 视为没写）。
	N int64
	// SeqAuto 表示 sequential_image_generation=auto（组图）。
	SeqAuto bool
	// MaxImages 是组图的 max_images（0 = 未指定，按上限冻结）。
	MaxImages int64
	// LayerDecomp 表示 layer_decomposition（图层拆分：底图 + 若干图层）。
	LayerDecomp bool
	// RefImages 是参考图张数（与组图额度共享上限）。
	RefImages int
	// MaxN 是该模型的单张上限；**0 = 未配置**。
	//
	// 未配置时上限按引擎上限（MaxImageN）算，而不是按 1：
	// 上限只用于"夹紧用户要的张数"，用户显式写 n=3 就该冻 3 张；
	// 而"用户没写 n"的默认值单独是 1 —— 一次普通请求按 128 张冻结会把余额直接打空。
	MaxN int64
}

// ImageCountHint 返回本次请求的冻结张数（>=1）。
//
// 分支顺序与"谁更大"无关，只与**语义优先级**有关：
//
//	n（用户显式下单）→ 图层拆分（一次调用的产物形态）→ 组图（max_images 或上限）
//
// 图层拆分压过组图：它是一次调用的产物形态（底图 + 图层），与"连续生成 N 张"互斥；
// 两者同时出现时按产物上限冻结，避免穿透。
func ImageCountHint(in ImageCountInput) int64 {
	// cap 是「一次能出几张」的硬上限；未配置 max_n 时用引擎上限。
	cap := in.MaxN
	if cap < 1 || cap > MaxImageN {
		cap = MaxImageN
	}
	// 默认 1 张：**只有用户没给任何张数意图时**才用它。
	n := in.N
	if n < 1 {
		n = 1
	}
	if in.LayerDecomp {
		// 图层拆分的产物数由模型决定（底图 + 各图层），按上限冻结。
		// 必须与组图分支互斥：曾经写成两个独立 if，SeqAuto 后写会把它覆盖掉
		// （`{图层,组图,MaxImages:4,MaxN:15}` 实际返回 4），而注释与测试名都声称
		// "图层压过组图"。冻结偏少会让余额闸门放宽，结算时差额由平台承担。
		n = cap
	} else if in.SeqAuto {
		if in.MaxImages > 0 {
			n = in.MaxImages
		} else {
			n = cap
		}
		// 参考图与生成数共享同一个额度（上游事实）：再要 N 张就得给参考图腾位置。
		if in.RefImages > 0 && n > cap-int64(in.RefImages) {
			if room := cap - int64(in.RefImages); room > 0 {
				n = room
			}
		}
	}
	if n > cap {
		n = cap
	}
	if n < 1 {
		n = 1
	}
	return n
}

// BillableCount 是「按件计价」落到最终计费条数的**唯一分派入口**。
//
// 为什么需要它：同一条规则此前由调用方各自 if/else 分派（结算走 BillableClips、
// 回收走 BillableImageN），于是 per_clip 的「0 条 = 免单」只在结算路径成立，
// 回收路径按 max_n 收 2 条 —— 同一单在两条路径上价格不同，用户可举证。
//
// 参数语义（第三个参数是**事实的可信度**，不是第二个 0）：
//   - known=false：交付量未知（回收期拿不到 artifact_count）→ 一律保持意图，
//     按冻结额收。这是回滚开关，也是「无证据不多收不少收」的基线。
//   - known=true：actual 是**事实**。actual=0 时 per_clip 免单、per_image 保持意图
//     （图片的零交付由调用方的「全失败闸门」转 Fail 全额释放，不走这里）。
//   - actual > intent：上游多给不向用户收费，按意图上限收。
func BillableCount(sp model.SpecPrices, intent, actual int64, known bool) int64 {
	if intent <= 0 {
		return intent
	}
	if !known {
		return intent
	}
	if IsPerClip(sp) {
		return BillableClips(intent, actual)
	}
	return BillableImageN(intent, actual)
}

// BillableImageN 是**按张计价**的计费张数：意图与交付取小。
//
// 调用方一般应走 BillableCount（它按 kind 分派）；本函数只在明确知道
// 「这一单按张计价」时直接调用。
//
// 三个出口共用：结算（task.applyArtifactActuals）、回收（ReclaimSettle）、
// 迟到补结（LateSettle）。口径曾经分叉：回收只做「actual < intent 才下调」，
// 而 intent（usages.image_n）在 pending 行上恒为 0（冻结期不落列），
// 于是永远不下调 —— 冻结 15 张、实际交付 2 张的会话被回收时，
// imageCharge 的 `n = max(1, 0)` 保底按 **1 张** 入账。
//
// 参数语义：
//   - intent <= 0：本单不按张数计价（音乐/视频/未落列的历史行）→ 原样返回，
//     绝不把 0 变成 1（那会让按任务计费的单子凭空多一笔）。
//   - actual <= 0：没有交付证据（不是"交付了 0 张"）→ 保持意图；
//     真正的零交付由调用方的「全失败闸门」转 Fail 全额释放，不走这里。
//   - actual > intent：上游多给不向用户收费，按意图上限收。
func BillableImageN(intent, actual int64) int64 {
	if intent <= 0 {
		return intent
	}
	if actual <= 0 || actual >= intent {
		return intent
	}
	return actual
}

// BillableClips 是**按条计价**（音乐/歌词）的计费条数。
//
// 调用方一般应走 BillableCount（它按 kind 分派）；本函数只在明确知道
// 「这一单按条计价」时直接调用。
//
// 与 BillableImageN 的关键区别只有一条，但它是整条规则的支点：
// **actual <= 0 时返回 0，而不是保持意图**。
//
// 为什么不能复用 BillableImageN：图片的"没有交付证据"（actual=0 且调用方没拿到
// 产物清单）应当保持意图、由全失败闸门去判失败；而音乐的上游会明确回「成功但
// 一首都没给出导出链接」，那是"确实交付了 0 条"，按意图 2 条收钱无法解释。
// 两者语义不同，共用一个函数就只能二选一，必然错一边。
//
// 参数语义：
//   - intent <= 0：本单没有条数意图（未落列的历史行）→ 原样返回；
//   - actual <= 0：交付 0 条 → **0**（免单）；
//   - actual > intent：上游多给不向用户收费，按意图上限收。
func BillableClips(intent, actual int64) int64 {
	if intent <= 0 {
		return intent
	}
	if actual <= 0 {
		return 0
	}
	if actual >= intent {
		return intent
	}
	return actual
}
