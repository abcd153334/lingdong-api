package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/pkg/uploadcfg"
	"lingdong-api/internal/service/storage"
	"lingdong-api/internal/service/upload"
)

// 本文件解决「OpenAI 原生 multipart 入参」与「上游只收公网直链」之间的落差。
//
// 真实场景：调用方按 OpenAI 规范 POST /v1/images/edits，文件放在 multipart 的 image 字段里；
// 而多米API 的图生图上游只接受 image_urls 数组里的 http(s) 直链，**不接受文件字节**。
// 平台中间必须把文件落成可访问的直链，否则这条端点在多米渠道上永远无法工作。
//
// 三个设计约束：
//  1. **只按适配器声明触发**：OpenAI 兼容渠道要的正是原始 multipart，替它转成 JSON 会 400。
//     是否转换由适配器实现 MultipartURLSupporter 显式表达，不按 adapter_id 硬编码。
//  2. **一次上传、多次复用**：failover 换渠道时不能重传同一份文件（重复占用用户上传配额）。
//  3. **不再按字段名过滤**：文本字段全部进 JSON，文件字段全部落成直链并按原字段名写入。
//     过去那张 7 键白名单会把 `seed`、`output_format` 之类静默吞掉（连 upstream_raw 都看不到）。

// MultipartURLSupporter 是适配器**可选**能力：声明某些 multipart 端点需要「文件转直链」。
//
// 为什么接口放在 relay 而不是让适配器自己转：
// 适配器的 Forward 只拿得到 ChannelConfig（渠道密钥）—— 没有用户、没有 KEY、
// 没有站点地址，也没有权限决定「这份素材算谁的上传配额、落在哪个存储」。
// 素材落库是平台的职责，适配器只该声明「我需要直链」。
type MultipartURLSupporter interface {
	// NeedsPublicURL 表示该端点的 multipart 文件必须先转成公网直链再下发。
	// 返回 false 的端点保持原始 multipart 透传（适配器自己处理文件）。
	NeedsPublicURL(kind EndpointKind) bool
}

const (
	// maxMultipartTextFieldBytes 是单个文本字段的上限。
	// prompt 上限 32000 字符（UTF-8 最多 4 字节）≈ 128 KiB，留一倍余量。
	maxMultipartTextFieldBytes = 256 << 10
	// maxMultipartFileFields 是文件字段数量上限（image 多图）。
	// 与 duomiMaxGPTImageRefs（16）对齐：再多的图上游也不收，早拒比晚拒省一次上传。
	maxMultipartFileFields = 16
)

// multipartMaterializer 把一个 multipart 请求体按需转换成「文件换成直链」的 JSON。
//
// 惰性求值是刻意的：只有真的走到「要求直链」的候选才会上传文件。
// 一份 edits 请求若全部落在 OpenAI 兼容渠道上，就不该产生任何上传记录。
type multipartMaterializer struct {
	body        []byte
	contentType string

	done     bool
	jsonBody []byte
	err      error
}

// forAdapter 返回「该候选适配器应该收到的请求体」。
//
// 三种情况：
//   - 端点本来就不是 multipart → 原样返回；
//   - 适配器不要求直链（含未实现接口的 OpenAI 兼容渠道）→ 原样返回；
//   - 适配器要求直链 → 返回转好的 JSON（首次调用时转换，之后复用）。
func (m *multipartMaterializer) forAdapter(ctx context.Context, c *gin.Context, a Adapter, kind endpointKind) ([]byte, string, error) {
	if !specOf(kind).multipart {
		return m.body, m.contentType, nil
	}
	s, ok := a.(MultipartURLSupporter)
	if !ok || !s.NeedsPublicURL(kind) {
		return m.body, m.contentType, nil
	}
	if !m.done {
		m.done = true
		m.jsonBody, m.err = m.materialize(ctx, c)
	}
	if m.err != nil {
		return nil, "", m.err
	}
	return m.jsonBody, "application/json", nil
}

// uploadCtx 把「鉴权上下文 + 站点地址」的解析推迟到第一次真正落文件时。
//
// 为什么延迟：字段白名单、文件数量、单文件大小都是**纯请求校验**，
// 必须在任何平台侧的解析之前给出 400。若一开始就取 KEY/User/站点地址，
// 平台配置问题（未配 site.api_base_url）会抢先盖掉「字段名不合法」这种
// 调用方一眼能改的错误，用户便无从下手。
type uploadCtx struct {
	c    *gin.Context
	k    *model.APIKey
	u    *model.User
	base string
	ok   bool
}

func (uc *uploadCtx) prepare() error {
	if uc.ok {
		return nil
	}
	if uc.c == nil {
		// 生产链路上 TokenAuth 一定先跑过；这里兜住的是「被别的代码路径误调」的 panic
		// —— panic 发生在 Freeze 之后会让冻结额度无人释放。
		return fmt.Errorf("multipart 素材落库缺少请求上下文")
	}
	k, u := APIKeyOf(uc.c), UserOf(uc.c)
	if k == nil || u == nil {
		return fmt.Errorf("multipart 素材落库缺少鉴权上下文")
	}
	base := publicBaseURL(uc.c)
	if base == "" {
		// 这是**平台配置缺失**，不是调用方的参数问题：归到内部错误，
		// 让运维按「站点设置 → API 调用地址未配置」排查，而不是让用户反复改请求。
		return fmt.Errorf("平台未配置对外访问地址（站点设置 → API 调用地址），无法为上传文件生成公网直链")
	}
	if !publicBaseReachable(base) {
		// 本地/内网地址上游一定取不到。此时提交任务只会拿到「任务失败（未返回原因）」，
		// 用户白等一轮、平台白付一次上游成本，而且错误完全不可归因——
		// 所以必须在这里就停：说清是哪个地址、该改哪里。
		return fmt.Errorf("平台对外地址 %s 是本机或内网地址，上游无法回取参考图；"+
			"请在「系统设置 → 站点」把 API 基址改成公网可访问的域名（当前请求 Host 不可用时不作回退）", base)
	}
	uc.k, uc.u, uc.base, uc.ok = k, u, base, true
	return nil
}

// materialize 解析 multipart、把文件落成直链、按白名单重建成 JSON。
func (m *multipartMaterializer) materialize(ctx context.Context, c *gin.Context) ([]byte, error) {
	_, params, perr := mime.ParseMediaType(m.contentType)
	if perr != nil || strings.TrimSpace(params["boundary"]) == "" {
		// 不是合法 multipart：报调用方参数错，不要猜格式硬转。
		return nil, &CallerParamError{Msg: "multipart 请求缺少 boundary（请按 OpenAI 规范以 multipart/form-data 提交）"}
	}
	up := &uploadCtx{c: c}

	// 参考图/素材只可能是图片：单文件上限直接取「图片类」的配置上限。
	// 外层请求体门（middleware.UploadBodyCap）恒 ≥ 它（那是所有类别的最大值 + 开销），
	// 所以这里不再需要 min() —— 改造前正是那个 min(配置, 25MiB) 把 API 侧压到了 25MB。
	maxFileBytes := uploadcfg.Current().LimitBytes("png")
	mr := multipart.NewReader(bytes.NewReader(m.body), params["boundary"])

	out := make(map[string]any, 12)
	var imageURLs []string
	fileFields := 0

	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, &CallerParamError{Msg: "multipart 请求体解析失败：" + err.Error()}
		}
		name := p.FormName()
		filename := p.FileName()

		// 无名 part 一律忽略（写进 JSON 会变成一个空键）。
		if name == "" {
			_, _ = io.Copy(io.Discard, io.LimitReader(p, maxMultipartTextFieldBytes))
			continue
		}

		if filename == "" {
			// 文本字段：全部进 JSON。不按字段名过滤 —— 平台不认识的键由上游决定收不收，
			// 吞掉的代价是「用户以为参数生效了」。
			buf, rerr := io.ReadAll(io.LimitReader(p, maxMultipartTextFieldBytes+1))
			if rerr != nil {
				return nil, &CallerParamError{Msg: fmt.Sprintf("读取字段 %s 失败：%v", name, rerr)}
			}
			if len(buf) > maxMultipartTextFieldBytes {
				return nil, &CallerParamError{Msg: fmt.Sprintf("字段 %s 过长（超过 %d 字节）", name, maxMultipartTextFieldBytes)}
			}
			out[name] = normalizeMultipartText(name, string(buf))
			continue
		}

		// 文件字段：落成公网直链后按**原字段名**写回 JSON（mask 例外，见下）。
		fileFields++
		if fileFields > maxMultipartFileFields {
			return nil, &CallerParamError{Msg: fmt.Sprintf(
				"文件数量过多（上限 %d 个）", maxMultipartFileFields)}
		}
		buf, rerr := io.ReadAll(io.LimitReader(p, maxFileBytes+1))
		if rerr != nil {
			return nil, &CallerParamError{Msg: fmt.Sprintf("读取文件 %s 失败：%v", name, rerr)}
		}
		if int64(len(buf)) > maxFileBytes {
			return nil, &CallerParamError{Msg: fmt.Sprintf(
				"文件 %s 超过上传上限 %d MB", name, maxFileBytes>>20)}
		}
		// 图片端点只收图片：上传域的白名单是**通用文件**（txt/csv/pdf/docx 都合法），
		// 放行到这里的结果是「txt 通过校验 → 送去对象存储 → Put 失败 → 503」，
		// 用户看到的是平台故障，还得白等十几秒（2026-11-04 实测 17.8s）。
		// 判据复用上传域的同一张魔数表，不另写一份。
		if family, danger := upload.DetectFamily(buf); danger || family != "image" {
			// 文案必须与实际放行的族一致（upload.ImageFileHint 与 sniff 的 image 分支同源），
			// 否则用户按提示换成 png 之外的合法格式时会被拒，且提示本身就在说谎。
			return nil, &CallerParamError{Msg: fmt.Sprintf(
				"%s 只接受图片文件（%s）", name, upload.ImageFileHint)}
		}
		// 到这里才需要鉴权上下文与站点地址：前面的校验全是调用方自己能修的错。
		if perr := up.prepare(); perr != nil {
			return nil, perr
		}
		url, uerr := storeUploadedFile(ctx, up.k, up.u, filename, buf, up.base)
		if uerr != nil {
			return nil, uerr
		}
		switch name {
		case "mask", "mask_url":
			// 蒙版文件字段：平台名是 mask_url；OpenAI 官方 form 字段叫 mask，
			// 两个写法都在这里落成 mask_url，出站报文里不会再出现 mask。
			out["mask_url"] = url
		case "image", "image[]":
			imageURLs = append(imageURLs, url)
		default:
			out[name] = url
		}
	}

	// 单图用字符串、多图用数组：与 OpenAI / 多米两侧的既有写法都兼容
	// （多米 duomiImageField 对两种形态都接受，单图发数组也无害，但单图发字符串更贴合上游示例）。
	switch len(imageURLs) {
	case 1:
		out["image"] = imageURLs[0]
	default:
		if len(imageURLs) > 1 {
			out["image"] = imageURLs
		}
	}
	return json.Marshal(out)
}

// normalizeMultipartText 把文本字段归一：n 转成数字，其余去首尾空白。
//
// n 必须是数字：上游若收到字符串 "2" 有的网关会当类型错，
// 而 multipart 里所有字段天然是字符串，必须在这里补回类型。
func normalizeMultipartText(name, raw string) any {
	v := strings.TrimSpace(raw)
	if name == "n" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
		return v
	}
	return v
}

// storeUploadedFile 把内存里的一份文件落成可被上游回取的地址。
//
// 复用 upload 域而不是自己拼存储路径：限额、魔数校验、保留期、到期清理
// 都在那一处维护，这里再写一份必然漂移（尤其是「谁的上传配额」这件事）。
// 地址与上传接口同源（storage.URLOr）：云端给对象存储公开地址，上游直取，
// 不再依赖"我们自己的域名能被上游访问到"（本地开发常踩：api_base_url 指向前端）。
func storeUploadedFile(ctx context.Context, k *model.APIKey, u *model.User, filename string, buf []byte, base string) (string, error) {
	rec, err := upload.Create(ctx, upload.CreateInput{
		UserID: u.ID, KeyID: k.ID, Filename: filename,
		Size: int64(len(buf)), Reader: bytes.NewReader(buf),
	})
	if err != nil {
		// 上传域的错误是**可归因的**：文件类型不支持 / 超过限额 / 当日配额用尽 / 总容量满。
		// 全部映射成调用方 400 并带回原文案，用户才知道该换图还是该清理。
		return "", &CallerParamError{Msg: "参考图上传失败：" + uploadErrText(err)}
	}
	return storage.URLOr(
		storage.Ref{Backend: rec.StorageBackend, Bucket: rec.StorageBucket, Key: rec.StorageKey},
		upload.BuildURL(base, rec.ID, rec.SafeName),
	), nil
}

// uploadErrText 把上传域错误翻成可执行的中文提示。
func uploadErrText(err error) string {
	switch {
	case errors.Is(err, upload.ErrDisabled):
		return "平台未开启文件上传，无法接收 multipart 文件；请改用公网直链形式（image_url）"
	case errors.Is(err, upload.ErrTypeUnsupported):
		return "图片格式不受支持（支持 png / jpg / jpeg / webp）"
	case errors.Is(err, upload.ErrTooLarge):
		return "图片超过单文件大小上限"
	case errors.Is(err, upload.ErrEmpty):
		return "图片为空文件"
	case errors.Is(err, upload.ErrDailyLimit):
		return "该 API KEY 今日上传次数已用尽"
	case errors.Is(err, upload.ErrConcurrency):
		return "上传并发过高，请稍后重试"
	case errors.Is(err, upload.ErrStorageFull):
		return "账号上传总容量已满，请清理历史文件"
	case errors.Is(err, upload.ErrStorageUnavailable):
		return "存储服务暂不可用，请稍后重试"
	}
	return err.Error()
}

// publicBaseReachable 判断 base 的 host 是否「上游有可能访问到」。
//
// 只拦截本机/内网/未指定地址，**不解析域名**：DNS 解析结果以本机视角为准，
// 与上游（第三方聚合平台）的视角可能不同，按解析结果判会把正常部署误判成不可用。
// 代价是「域名解析到内网」这类配置查不出来 —— 但那种情况有明确的失败文案（上游任务失败），
// 比把线上正常配置拦成 500 更划算。
func publicBaseReachable(base string) bool {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return false
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast())
	}
	return true
}

// publicBaseURL 返回对外可访问的站点基址（用于拼直链）。
//
// 逻辑集中在 pkg/siteurl（那里是唯一推导点）；这里保留薄封装，
// 是因为 relay 侧只关心「能不能拼出上游可访问的地址」，不关心推导细节。
//
// **必须取第一条（ConfiguredBase），不能跟随入口线路**：这个地址是交给
// 第三方去回取的。跟着线路走会把"某条线路下线"变成"上游取图 404"。
// 配置为空时保持旧行为（回落请求 Host）—— 只有本地开发会走到这里，
// 集群模式下 storage.URLOr 已经直接返回对象存储地址。
func publicBaseURL(c *gin.Context) string {
	if b := siteurl.ConfiguredBase(); b != "" {
		return b
	}
	return siteurl.Base(c)
}
