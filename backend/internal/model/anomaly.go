package model

import "strings"

// usages.anomaly 的标记全集（逗号分隔，落库前列序稳定）。
//
// 为什么必须常量化：这些字符串是**对账判据**。改造前 22 个标记以字面量散落在
// billing / relay / task / reconcile / logs 五处，`reconcile` 用 LIKE '%reclaimed%'
// 匹配、`logs/portal.go` 用 == "reclaimed" 过滤。任何一处拼错都不会报错，
// 只会让资金异常从看板上静默消失。
const (
	// AnomalyChargeCapped 表示结算被上限截顶（charge_before_cap 记录了截顶前的应收额）。
	AnomalyChargeCapped = "charge_capped"
	// AnomalyNegativeBalanceBlocked 表示余额下限拦截（应收被压到可用余额）。
	AnomalyNegativeBalanceBlocked = "negative_balance_blocked"
	// AnomalyCacheOverlapClamped 表示 prompt 扣除缓存/图片/音频通道后为负，已钳到 0。
	AnomalyCacheOverlapClamped = "cache_overlap_clamped"
	// AnomalyNegativeInput 表示上游上报了负 token，该通道已归零。
	AnomalyNegativeInput = "negative_input"
	// AnomalyReclaimed 表示孤儿 pending 由回收 worker 保守补收。
	AnomalyReclaimed = "reclaimed"
	// AnomalyReclaimedAfterSuccess 表示任务**已成功且有产物**、但结算没跑完，
	// 由回收 worker 按估算补结（不是退款）。
	//
	// 与 reclaimed 的区别关键在处置方向：回收器曾把「任务终态」一律当失败退款，
	// 结果用户下载到产物却一分钱没付。这条标记让「补结」与「退款」在对账里可分辨。
	AnomalyReclaimedAfterSuccess = "reclaimed_after_success"
	// AnomalyReclaimedEmptySuccess 表示任务标记成功但**没有任何产物**，
	// 回收 worker 按「无交付」全额退款。
	AnomalyReclaimedEmptySuccess = "reclaimed_empty_success"
	// AnomalyReclaimedOrphan 表示连任务行都找不到（进程崩溃/任务被清理），
	// 按估算用量入账并把原因留痕。
	AnomalyReclaimedOrphan = "reclaimed_orphan"
	// AnomalyReclaimedRetrySuperseded 表示这一单是**被重试取代的旧会话**：
	// 任务已改用新会话，旧冻结只能退、绝不能再收。
	//
	// 它必须是独立标记：与 reclaimed_orphan 混在一起时，对账无法区分
	// 「该退的旧单」与「该收的孤账」。
	AnomalyReclaimedRetrySuperseded = "reclaimed_retry_superseded"
	// AnomalyReclaimedNoEvidence 表示无任务且**没有任何上游受理证据**
	// （upstream_request_id 为空）。最常见来源是 Freeze 与建任务之间崩溃——
	// 上游一定没被调用，按冻结额收就是纯多收。
	AnomalyReclaimedNoEvidence = "reclaimed_no_evidence"
	// AnomalyReclaimedUpstreamCostLost 表示任务失败但上游受理过（有上游单号）：
	// 用户全额退款，平台很可能已付上游成本。金额不动，只给对账一个可统计的分子。
	AnomalyReclaimedUpstreamCostLost = "reclaimed_upstream_cost_lost"
	// AnomalyCompletionUnmeasured 表示回收时无法测得 completion，按估算值结算。
	AnomalyCompletionUnmeasured = "completion_unmeasured"
	// AnomalyLateSuccessCharged 表示先释放、后收到成功回调的迟到补收。
	AnomalyLateSuccessCharged = "late_success_charged"
	// AnomalyLateSuccessFree 表示迟到成功但**一分钱都没补收**（charge 算出 0）。
	//
	// 与 late_success_charged 分开：那个表示「补收到了」，这个表示「交付了但没收钱」。
	// 免费模型是正常的；余额已耗尽、公式算出 0 则需要人工判断。
	AnomalyLateSuccessFree = "late_success_free"
	// AnomalyLateSuccessAfterReclaim 表示迟到成功时账已被回收 worker 结算过（ReclaimSettle），
	// 本次只回填 tasks.settled，不重复扣款。
	//
	// 这条标记本身就是对账线索：它意味着「同一个任务既被判过失败、又被收过钱」，
	// 说明回收与上游成功在时间上撞了，需要复核回收口径是否过激。
	AnomalyLateSuccessAfterReclaim = "late_success_after_reclaim"
	// AnomalyLateSuccessUsageArchived 表示迟到成功时 usages 行已被归档删除 → 不扣款、照常交付。
	//
	// 必须留痕：这是「交付了但确定收不到钱」的唯一记录，
	// 没有它这笔损失只能在上游成本报表里以总额形式出现。
	AnomalyLateSuccessUsageArchived = "late_success_usage_archived"
	// AnomalyLateSuccessUserDeleted 表示迟到成功时用户已软删，仍照常补扣（账要平）。
	AnomalyLateSuccessUserDeleted = "late_success_user_deleted"
	// AnomalyLateSuccessUserGone 表示迟到成功时用户行已物理删除 → 无处记账，不扣款但照常交付。
	AnomalyLateSuccessUserGone = "late_success_user_gone"
	// AnomalyLateSuccessRaceLost 表示复查已拿到产物，但 CAS 推进状态失败
	// （用户抢先 Retry、或被别处判终态）→ 不扣款、不交付。
	//
	// 这是「宁可少收也不多收」的取舍留下的痕迹：先 CAS 后扣款，
	// CAS 输掉的代价只是一次白下载，账上一分未动。
	AnomalyLateSuccessRaceLost = "late_success_race_lost"
	// AnomalySubmitAmbiguous 表示异步任务提交结果未知（网络中断/超时），需对账。
	AnomalySubmitAmbiguous = "submit_ambiguous"
	// AnomalyCancelRace 表示取消与完成竞态（已关闭的会话被追加标记）。
	AnomalyCancelRace = "cancel_race"
	// AnomalySubmitOrphaned 表示上游已建单、但任务状态推进失败导致单号一度丢失。
	//
	// 与 submit_ambiguous 的区别：那个是「不知道上游有没有受理」，这个是**确定受理了**
	// （拿到了 upstream_task_id），只是本地状态没推进成功。两者的处置不同：
	// 前者不能再投（可能重复建单），后者必须把单号补记进库、靠复查窗口把产物捞回来。
	// 没有这个标记时，这类任务会以「排队中」的样子被 orphan 判失败并全额退款，
	// 而上游真实计费 —— 只在月底「上游成本 > 平台收入」里体现，无法定位到单。
	AnomalySubmitOrphaned = "submit_orphaned"
	// AnomalyBodyTruncated 表示响应体被截断，usage 只能估算。
	AnomalyBodyTruncated = "body_truncated"
	// AnomalyUsageEstimated 表示这次用量是估算值而非上游实测。
	AnomalyUsageEstimated = "usage_estimated"
	// AnomalyOutputUnmeasured 表示输出 token 无法测量。
	AnomalyOutputUnmeasured = "output_unmeasured"
	// AnomalyClientAbort 表示客户端断连，completion 未置 1。
	AnomalyClientAbort = "client_abort"
	// AnomalyZeroUsage 表示上游返回零用量（可能白嫖，需观察）。
	AnomalyZeroUsage = "zero_usage"
	// AnomalyImageCountZero 表示上游返回 0 张图。
	AnomalyImageCountZero = "image_count_zero"
	// AnomalyImageResponseFailed 表示上游以「200 + 顶层 error」明确告知整单没出图，
	// 本次按 0 张结算并全额释放（不是按 1 张兜底）。
	//
	// 与 image_count_zero 的区别：那条是「我们看不懂响应」（仍按 1 张收，避免白送），
	// 这条是「上游明确失败」（一张都不该收）。混成一个标记就分不清
	// 「平台解析缺字段」和「上游真的没产出」，前者要改代码、后者要找上游。
	AnomalyImageResponseFailed = "image_response_failed"
	// AnomalyImagePartialError 表示已交付 ≥1 张，但响应顶层同时带了 error（上游契约违约）。
	//
	// 交付与金额都按实际张数走，只需留痕：它意味着上游把「部分失败」写进了
	// 顶层 error 字段，而文档定义该字段为「整个请求未产出任何图片」。
	AnomalyImagePartialError = "image_partial_error"
	// AnomalyImagePartialAllFailed 表示 0 张，且 data[] 里每项都带 error（组图全失败）。
	AnomalyImagePartialAllFailed = "image_partial_all_failed"
	// AnomalyImageNUnknown 表示 0 张且请求没写 n（上游既没给图也没给 failed 线索）。
	//
	// 以前这条落进 buildBillingUsage 的 default 分支，**连标记都不打**：
	// 用户什么都没拿到、被收 1 张，而调用记录看起来完全正常。
	AnomalyImageNUnknown = "image_n_unknown"
	// 视频档位兜底（参数不再拦截后，"按兜底口径计费"必须留证）：
	//
	//   - VideoTierUnpriced：用户写的档位价目里没有（按固定档/最高价档计费）；
	//   - VideoTierMissing：用户没写档位（按固定档/最高价档计费）。
	//   未传时长用既有的 AnomalyVideoSecondsMissing（语义完全相同，不另立一个）。
	AnomalyVideoTierUnpriced = "video_tier_unpriced"
	AnomalyVideoTierMissing  = "video_tier_missing"
	// AnomalyImageTierUnpriced 表示用户写的档位不在该模型价目里。
	//
	// 参数不再被拦截，所以这类请求会照常出站；平台按**最高价档**计费（不按低价糊弄），
	// 这条标记是事后核对「这个档位要不要补价」的唯一线索。
	AnomalyImageTierUnpriced = "image_tier_unpriced"
	// AnomalyImagePixelsOutOfWindow 表示请求像素落在「价目最高档面积×2」之外。
	AnomalyImagePixelsOutOfWindow = "image_pixels_out_of_window"
	// AnomalyImageTierConflict 表示档位与像素各自指向不同的档，已按单价更高者计费。
	AnomalyImageTierConflict = "image_tier_conflict"
	// AnomalyImageRatioConflict 表示 aspect_ratio 与像素比例不一致，已按像素推导画幅。
	AnomalyImageRatioConflict = "image_ratio_conflict"
	// AnomalyImageSizeConflict 表示 size 与 image_size / resolution 的值不一致，
	// 已按 size 计费（契约字段优先）。
	AnomalyImageSizeConflict = "image_size_conflict"
	// AnomalyTierUpgradeCharged 表示结算时按上游实际交付的更高档补收（只升不降）。
	AnomalyTierUpgradeCharged = "tier_upgrade_charged"
	// AnomalyVideoSecondsMissing 表示按秒计费但拿不到时长，已按冻结额入账。
	AnomalyVideoSecondsMissing = "video_seconds_missing"
	// AnomalyImageMaxNUnset 表示模型没有配置 max_n：冻结按 1 张、结算按实际交付，
	// 上限由引擎上限兜住。留着这条是为了让「漏配 max_n」可被直接筛出来。
	AnomalyImageMaxNUnset = "image_max_n_unset"
	// AnomalyBillingModeCoerced 表示 models.billing_mode 不是 spec，但价目里确实有
	// 按规格计价的数据（tier_price / second_price / …），运行期按 spec 计费。
	//
	// 成因几乎都是「billing_mode 落在列默认值 tokens 上」：配好了价目却完全不读。
	AnomalyBillingModeCoerced = "billing_mode_coerced"
	// AnomalySpecKindInferred 表示 spec_prices.kind 缺失或与价目形状不符，
	// 运行期按价目形状推断出真实 kind 后计费。
	AnomalySpecKindInferred = "spec_kind_inferred"
	// AnomalySpecKindConflict 表示价目形状与**请求端点**的模态不符
	// （视频端点却只有 tier_price），已按端点定 kind —— 结果是"本端点无可用价目"
	// 的明确报错，而不是用另一种模态的公式乱收钱。
	AnomalySpecKindConflict = "spec_kind_conflict"
	// AnomalyUpstreamBodyError 表示「HTTP 2xx 但响应体是错误信封」：
	// 已按失败处理（全额释放、不扣费），留痕供对账区分「上游 200 骗人」与真实成功。
	AnomalyUpstreamBodyError = "upstream_body_error"
	// AnomalyWeakParamPricingConflict 表示请求在**提示词**里带了计价类弱校验参数
	// （`--rs` / `--dur` / `--frames`），与 body 的计费参数不一致或 body 未给。
	//
	// 官方文档明确这类后缀「所有模型均兼容」且会生效，而平台只按 body 计费：
	// 结果是「按 720p 收钱、上游按 4k 出片」。本轮不拦不剥（会打断线上脚本），
	// 这条标记是事后筛出这类调用、并按上游交付档对账的唯一线索。
	AnomalyWeakParamPricingConflict = "weak_param_pricing_conflict"
	// AnomalyImageNMismatch 表示请求 n 与上游实际产出张数不一致。
	AnomalyImageNMismatch = "image_n_mismatch"
	// AnomalyImageNOverDelivered 表示上游产出多于用户下单张数，多出的部分**未计费**。
	//
	// 与 image_n_mismatch 的区别：那条只说"不一致"，这条明确了处置方向
	// （按用户下单张数收费），对账时能直接算出「平台替用户承担了多少张」。
	AnomalyImageNOverDelivered = "image_n_over_delivered"
	// AnomalyTierMismatch 表示上游实际交付尺寸与计费档位不一致（按 4K 收、按 2K 交）。
	//
	// 只告警不改价：改价需要先有稳定的「上游尺寸」数据，否则会把
	// 「上游没报尺寸」误判成不一致，变成一批假告警。
	AnomalyTierMismatch = "tier_mismatch"
	// AnomalyImageRatioMismatch 表示上游交付的画幅与请求声明的比例不一致。
	//
	// 与 tier_mismatch 分开：档位一致但比例被上游改掉（要 16:9 给 1:1）时金额没有问题，
	// 但交付不符预期；混成一个标记，就分不清"多收钱"和"交付错"。
	AnomalyImageRatioMismatch = "image_ratio_mismatch"
	// AnomalyImageSpecUnrecognized 表示请求的 size 既不是档位也不是像素/比例，已按默认档计费。
	//
	// 这类值会原样透传给上游（非标渠道可能认它），所以不能 400；但必须留痕，
	// 否则「按默认档收钱、上游按别的东西出图」没有任何线索。
	AnomalyImageSpecUnrecognized = "image_spec_unrecognized"
	// AnomalyUpstreamSpecUnparsable 表示上游回报的尺寸串无法解析或超长，已丢弃不落库。
	//
	// 落库前丢弃而不是截断：截断后的 "2048x11" 会被当成合法尺寸参与比对，
	// 比"没有这个值"更糟。
	AnomalyUpstreamSpecUnparsable = "upstream_spec_unparsable"
	// AnomalyImageNCapped 表示结算期张数超过模型上限，已按上限饱和计费。
	//
	// 与 charge_capped 分开：后者是"金额被截顶"（风控），这个只是"张数被按上限算"。
	// 混成一个标记，就分不清"少收了钱"和"少算了量"。
	AnomalyImageNCapped = "image_n_capped"
	// AnomalyStreamPartialDelivered 表示图片流式「上游生成张数 > 实际下发张数」。
	//
	// 场景：上游 completed 帧报 generated_images=3，但流在中途断开，用户只收到 2 张。
	// 结算按**用户拿到的**张数收费（不转嫁差额），这个标记是事后与上游对账、
	// 以及判断「这条渠道的流式是否稳定」的唯一线索。
	AnomalyStreamPartialDelivered = "stream_partial_delivered"
	// AnomalyStreamJSONFallback 表示客户端要流式、上游却回了整段 JSON（不是 SSE）。
	//
	// 场景：模型/渠道实际不支持流式，或上游忽略了 stream=true。此时上游已经出图并计费，
	// 平台必须按整段 JSON 交付（否则用户付钱拿到 502）。
	// 这条标记是「客户端按流写、实际拿到的不是流」的唯一可筛痕迹：
	// 它同时说明该模型的 image_stream 能力声明与实际不符，需要运营去改能力或换渠道。
	AnomalyStreamJSONFallback = "stream_json_fallback"
	// AnomalyAudioSecondsUnknown 表示按秒计费的音频拿不到真实时长，只能按最低消费兜底。
	AnomalyAudioSecondsUnknown = "audio_seconds_unknown"
	// AnomalyAudioSecondsEstimated 表示结算期拿不到音频真实时长，已按文本估算秒数计费。
	//
	// 与 audio_seconds_unknown 分开：那个是"按最低消费收"（可能是 0 元），
	// 这个是"按估算收"（有金额但秒数不精确）。对账时两者的处置完全不同：
	// 前者要查为什么探测失败、有没有漏收；后者只需确认误差是否在可接受范围。
	AnomalyAudioSecondsEstimated = "audio_seconds_estimated"
	// AnomalyUpstreamImageCountMismatch 表示上游 usage 的图片数与产物数不一致。
	AnomalyUpstreamImageCountMismatch = "upstream_image_count_mismatch"
	// AnomalyUsageUpstreamHigher 表示上游 usage 高于请求参数（按上游计费）。
	AnomalyUsageUpstreamHigher = "usage_upstream_higher"
	// AnomalyUsageUpstreamLower 表示上游 usage 低于请求参数（仍按请求计费，留痕观察）。
	AnomalyUsageUpstreamLower = "usage_upstream_lower"
	// AnomalyUsageUpstreamInputHigher 表示上游上报的输入（参考视频）秒数高于请求值。
	AnomalyUsageUpstreamInputHigher = "usage_upstream_input_higher"
	// AnomalyUsageUpstreamMissing 表示视频声明了智能时长（或未传时长）并按上限冻结，
	// 但上游既没返回 duration 也没返回 frames —— 结算只能回落到冻结预算。
	//
	// 不复用 output_unmeasured：那个是 token 语义（输出 token 无法测量），混用之后
	// 对账用同一个标记既筛「token 缺失」又筛「视频秒数缺失」，两类要查的东西完全不同。
	AnomalyUsageUpstreamMissing = "usage_upstream_missing"
	// AnomalyFrozenDrift 表示 users.frozen 小于会话冻结额（数据漂移，只按实际冻结回补）。
	AnomalyFrozenDrift = "frozen_drift"
	// AnomalyCapOverflow 表示上限计算溢出（不启用该级上限，按次一级上限处理）。
	AnomalyCapOverflow = "cap_overflow"
	// AnomalyUnpricedLane 表示上游报了未配价通道的 token（按 0 计并告警）。
	AnomalyUnpricedLane = "unpriced_lane"
	// AnomalyChargeCalcFailed 表示结算时算不出金额（快照损坏/价目缺失/档位不匹配/金额溢出），
	// 已**按预扣冻结额入账**。
	//
	// 这个标记的语义是「金额来源不是公式，而是冻结额」，对账必须按它筛出来人工复核：
	// 冻结额是事前估算，可能高于实际用量（多收）也可能低于（少收）。
	// 之所以不能改成「报错不落账」：usages 会停在 pending —— 同步请求冻结永不归还、
	// 异步任务卡到 deadline 全额退款（产物已交付 = 白送上游），回收 worker 还会
	// 每 5 分钟重试同一个必败运算。
	AnomalyChargeCalcFailed = "charge_calc_failed"
	// AnomalyStreamInterrupted 表示流式已开始输出后上游中断（连接断开/超时/错误帧）。
	//
	// 这批记录今天是 `http_status=200 + error_msg 空`，账单上显示「完美成功」，
	// 而渠道健康度却记了一次上游失败 —— 两边互相矛盾，排障时无法解释。
	// 打标后可以筛出「用户只拿到半条流」的账单，也能解释为什么 charge 只是部分金额。
	AnomalyStreamInterrupted = "stream_interrupted"
	// AnomalyResponseShortWrite 表示响应**部分送达**后写失败（对端关闭/反代读超时）。
	//
	// 与「一个字节都没写出去」必须分开：后者全额释放（用户什么都没拿到），
	// 前者用户已经拿到前 n 字节（图片前几帧/音频前段可用），按实测收费才不亏平台。
	// 打标后客服与对账能区分「完整交付」与「部分送达」，避免把正常收费看成多收。
	AnomalyResponseShortWrite = "response_short_write"
	// AnomalyUsageSemanticUnknown 表示上游给了 token 用量但没标语义（openai/anthropic），
	// 而这次又带了缓存/图片/音频通道 —— 计费按 OpenAI 语义解释。
	//
	// 语义判错会让 base 少算（Anthropic 的 input 不含缓存，按 OpenAI 解释会被再减一次
	// 缓存并钳 0）。这里不改计费分支（上游没标就只能猜），但必须留痕：
	// 否则「某条渠道一直在少收」永远不会有人发现。
	AnomalyUsageSemanticUnknown = "usage_semantic_unknown"
	// AnomalyStreamConvertFailed 表示入站格式转换在流式编码阶段失败（协议转换器内部错误）。
	//
	// 与 stream_interrupted 分开：那个是上游/网络的问题，这个是**我们的**转换器问题，
	// 归因不同（一个查渠道、一个查代码）。
	AnomalyStreamConvertFailed = "stream_convert_failed"
)

// 负 token 通道标记：`negative_` + 通道名。是 AnomalyNegativeInput 的明细伴生项，
// 用于回答"哪个通道被归零了"。清单必须与 billing.sanitizeUsage 的 clamp 调用一一对应。
const (
	AnomalyNegativePrompt       = "negative_prompt"
	AnomalyNegativeCompletion   = "negative_completion"
	AnomalyNegativeCache        = "negative_cache"
	AnomalyNegativeCacheWrite5m = "negative_cache_write5m"
	AnomalyNegativeCacheWrite1h = "negative_cache_write1h"
	AnomalyNegativeImage        = "negative_image"
	AnomalyNegativeAudioInput   = "negative_audio_input"
	AnomalyNegativeAudioOutput  = "negative_audio_output"
)

// HasAnomaly 报告逗号分隔的标记串里是否含某个标记（精确匹配一项，不做子串匹配）。
//
// 唯一判定入口：取代 reconcile / logs / task 里各自的 LIKE '%x%' 与 == "x"。
// 用精确匹配而不是子串，是因为 `image_n_mismatch` 含 `image_n` 这类前缀关系，
// 子串匹配会让一个标记被误判成另一个。
func HasAnomaly(joined, want string) bool {
	if joined == "" || want == "" {
		return false
	}
	for _, f := range strings.Split(joined, ",") {
		if strings.TrimSpace(f) == want {
			return true
		}
	}
	return false
}

// JoinAnomalies 拼接标记（去重、保持首次出现顺序），返回逗号分隔串。
func JoinAnomalies(flags ...string) string {
	seen := make(map[string]bool, len(flags))
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		for _, part := range strings.Split(f, ",") {
			part = strings.TrimSpace(part)
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	return strings.Join(out, ",")
}
