# 图片生成（即梦格式）

> `POST /v1/images/generations` · Seedream 原生报文：字段名、默认值、取值范围与厂商《图片生成 API》逐条对齐，平台只替换 model。只有声明了「原生参数」能力的模型可用；其余模型请用「原生OpenAI格式」那篇。
>
> 自动生成 · 契约版本 2 · 接口文档 `ep-images-generations--jimeng`

## 0. 概览

Seedream 原生报文：字段名、默认值、取值范围与厂商《图片生成 API》逐条对齐，平台只替换 model。只有声明了「原生参数」能力的模型可用；其余模型请用「原生OpenAI格式」那篇。

- **Base URL**：`https://api.example.com`（`example.com` 是占位域名，换成平台实际地址）
- **接口**：`POST /v1/images/generations`

```bash
curl -X POST \
  "https://api.example.com/v1/images/generations" \
  -H "Authorization: Bearer $LD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "<model>",
  "prompt": "赛博朋克风格的城市夜景，霓虹灯与雨后倒影",
  "size": "2K",
  "watermark": false
}'
```

## 1. 鉴权

所有接口都用一个 Bearer Token 鉴权：

```
Authorization: Bearer <你的 API KEY>
```

- KEY 只放服务端；不要写进前端代码、不要提交进代码仓库。
- KEY 无效、过期或被禁用 → `401 invalid_api_key`。
- KEY 没有该模型权限 → `403 model_not_allowed`。

```bash
curl "https://api.example.com/v1/models" -H "Authorization: Bearer $LD_API_KEY"
```

## 2. 请求

| 项目 | 值 |
| --- | --- |
| 方法 | `POST` |
| 路径 | `/v1/images/generations` |
| 请求体 | application/json |

这是**同步**接口：请求在这一次调用里直接返回结果，没有任务号，也不需要轮询。

## 3. 参数

| 字段 | 类型 | 必填 | 默认 | 取值 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `model` | string | 必填 | — | doubao-seedream-5-0-260128 | 平台模型 ID；出站时替换成厂商侧的真实模型名 |
| `prompt` | string | 条件必填：图片生成场景必填；图层拆分场景可省（不传则自动识别图中主要元素并拆分）。 | — | — | 画面描述或编辑指令；交互编辑在这里写 <bbox> 坐标。图层拆分场景可省；字数建议：中文 ≤ 300 字、英文 ≤ 600 词。 交互编辑（仅 Seedream 5.0 pro / flash）：在提示词里用 <bbox>x1 y1 x2 y2</bbox> 标出要编辑的区域，坐标是原图像素（左上角为原点），可写多个框；模型只改框内，其余保持不变 |
| `image` | any | 选填 | — | — | 输入图：交互编辑、图层拆分、图生图都用它。公网 URL 或 data:image/<格式>;base64,…；单张传字符串，多张传数组；张数上限、单张体积、支持的格式与宽高比范围随模型不同，以厂商官方文档为准（平台原样透传，不做数量与格式校验）。 图层拆分场景按官方要求传原图（通常 1 张） |
| `size` | string | 选填 | 随场景 | — | 图像尺寸：档位名，或「宽x高」像素值；平台**原样透传**：写档位就发档位、写像素就发这一串，不换算、不改写、不校验。该模型接受哪些写法（档位 / 像素 / auto）、像素上下限与宽高比范围，以厂商官方文档为准。本模型开放的档位见文末「档位」一节 |
| `response_format` | string | 选填 | url | url / b64_json | 返回链接还是内嵌 base64；url 的链接 24 小时后失效，需要长期保存请自行转存 |
| `output_format` | string | 选填 | jpeg | png / jpeg | 输出图像格式；支持模型：5.0 pro / flash / lite。图层拆分只控制底图，图层恒为 png |
| `background` | string | 选填 | opaque | transparent / opaque | 透明通道；支持模型：5.0 pro / flash。transparent 仅图生图且只能输入 1 张带透明通道的图；同时 output_format=jpeg 会报错 |
| `watermark` | boolean | 选填 | true | — | 是否在图片右下角加「AI 生成」水印；平台按原样提交，不做校验；是否生效由模型提供方决定 |
| `sequential_image_generation` | string | 选填 | disabled | auto / disabled | 组图模式；支持模型：5.0 lite / 4.5 / 4.0。5.0 pro / flash 只出单图，传 auto 无效。auto 由模型自行判断是否出组图及张数；disabled 只出一张 |
| `sequential_image_generation_options` | object | 选填 | — | — | 组图配置，仅 sequential_image_generation=auto 时生效；支持模型：5.0 lite / 4.5 / 4.0 |
| `sequential_image_generation_options.max_images` | integer | 选填 | 15 | — | 本次最多生成几张；范围 [1, 15]，默认 15；参考图数量 + 生成张数 ≤ 15；1 ~ 15 |
| `optimize_prompt_options` | object | 选填 | — | — | 提示词优化配置 |
| `optimize_prompt_options.mode` | string | 选填 | standard | standard / fast | 优化模式；standard 全模型可用；fast 更快、效果略低，仅 5.0 pro 支持（flash / lite / 4.5 不支持） |
| `tools` | array | 选填 | — | — | 工具列表（目前只有联网搜索）；支持模型：5.0 lite。写法 [{type: "web_search"}]；实际搜索次数见响应 usage.tool_usage.web_search |
| `tools[].type` | string | 必填 | — | web_search | 工具类型 |
| `layer_decomposition` | boolean | 选填 | false | — | 图层拆分开关：把 1 张图拆成 1 张底图 + 最多 16 个可独立编辑的图层；支持模型：5.0 pro / flash。只能输入 1 张图；任一图层失败即整单报错，不支持部分成功；拆分数量超过上限可能丢图层 |
| `stream` | boolean | 选填 | false | — | 流式输出，逐张返回半成品与成品；支持模型：5.0 lite / 4.5 / 4.0。开启后响应是 SSE（image_generation.partial_succeeded / completed），不是一段 JSON |
| `async` | boolean | 选填 | false | — | 是否异步：true 时立即返回任务对象，用 /v1/tasks/{id} 查询；异步任务与同步出图的字段完全一致，只是不等结果；轮询与取产物见同族的「查询任务 / 下载产物 / 取消任务」 |

## 4. 响应

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `created` | integer | 创建时间（Unix 秒） |
| `model` | string | 本次实际使用的模型名 |
| `data[].url` | string | 图片地址（response_format=url 时返回，24 小时有效） |
| `data[].b64_json` | string | 图片 base64（response_format=b64_json 时返回） |
| `data[].size` | string | 实际出图尺寸，形如 2048x2048 |
| `data[].output_format` | string | 实际输出格式（png / jpeg） |
| `data[].z_index` | integer | 图层叠放顺序：底图固定 0，图层从 1 递增，数值越大越靠上 |
| `data[].name` | string | 图层名称：模型按主体内容自动生成 |
| `data[].description` | string | 图层描述：比 name 更细的颜色 / 状态 / 材质等 |
| `data[].bounding_box` | object | 图层在输出底图坐标系中的边界框；底图覆盖整张画布，不返回该字段；absolute 与 normalized 二选一即可：要还原到底图用 absolute，要还原到任意画布用 normalized |
| `data[].bounding_box.absolute` | array | 绝对像素坐标 [left, top, right, bottom]，原点为底图左上角；还原：x=left，y=top，w=right-left，h=bottom-top；把图层缩放到 w×h 后放到 (x, y)，多个图层按 z_index 从小到大叠放 |
| `data[].bounding_box.normalized` | array | 归一化坐标 [left, top, right, bottom]，按底图宽高换算到 [0, 1000]；还原到目标画布 W×H：x=left/1000×W，y=top/1000×H，w=(right-left)/1000×W，h=(bottom-top)/1000×H。整数换算可能有取整误差 |
| `data[].error` | object | 组图场景下某一张失败时，这张图的错误信息；审核不通过会继续生成其余图片；内部服务异常（500）会终止后续生成 |
| `data[].error.code` | string | 该图的错误码 |
| `data[].error.message` | string | 该图的错误消息 |
| `usage.generated_images` | integer | 成功生成的张数（不含失败） |
| `usage.input_images` | integer | 输入模型的图片张数（5.0 pro / flash） |
| `usage.output_tokens` | integer | 出图消耗的 token：sum(长×宽)/256 取整 |
| `usage.total_tokens` | integer | 本次总 token；当前不计算输入，故与 output_tokens 相同 |
| `usage.tool_usage.web_search` | integer | 联网搜索次数（仅 5.0 lite，0 表示未搜） |

**200 成功（同步出图）**

- data[] 里是本次生成的图片或图层。
```json
{
  "created": 0,
  "model": "<string>",
  "data": [
    {
      "url": "<string>",
      "b64_json": "<string>",
      "size": "<string>",
      "output_format": "<string>",
      "z_index": 0,
      "name": "<string>",
      "description": "<string>",
      "bounding_box": {
        "absolute": [],
        "normalized": []
      },
      "error": {
        "code": "<string>",
        "message": "<string>"
      }
    }
  ],
  "usage": {
    "generated_images": 0,
    "input_images": 0,
    "output_tokens": 0,
    "total_tokens": 0,
    "tool_usage": {
      "web_search": 0
    }
  }
}
```


**202 已受理（async: true）**

- 返回任务对象；用 id 调 GET /v1/tasks/{id} 查询进度。
```json
{
  "id": "task_01J9HA7JPQ9A0Z6JZ3V8M9W6PZ",
  "object": "task",
  "status": "queued",
  "model": "MiniMax-H3",
  "modality": "video",
  "kind": "video",
  "model_icon_url": "https://api.example.com/icons/minimax.png",
  "progress": 0,
  "created_at": 1762318830,
  "completed_at": 1762319000,
  "deadline_at": 1762322400,
  "error": {
    "message": "任务处理失败，已全额返还"
  },
  "contents": [
    {
      "index": 0,
      "url": "https://api.example.com/v1/tasks/task_01J9HA7JPQ9A0Z6JZ3V8M9W6PZ/contents/0",
      "content_type": "video/mp4",
      "bytes": 3145728,
      "kind": "video"
    }
  ],
  "usage": {
    "images": 0,
    "seconds": 5,
    "ref_images": 0
  }
}
```


## 5. 示例代码

```bash
curl -X POST \
  "https://api.example.com/v1/images/generations" \
  -H "Authorization: Bearer $LD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "<model>",
  "prompt": "赛博朋克风格的城市夜景，霓虹灯与雨后倒影",
  "size": "2K",
  "watermark": false
}'
```

```python
import os
import requests

BASE = "https://api.example.com"
HEADERS = {"Authorization": "Bearer " + os.environ["LD_API_KEY"]}

payload = {
    "model": "<model>",
    "prompt": "赛博朋克风格的城市夜景，霓虹灯与雨后倒影",
    "size": "2K",
    "watermark": False,
}

r = requests.post(BASE + "/v1/images/generations", headers=HEADERS, json=payload, timeout=(10, 300))
r.raise_for_status()
print(r.json())
```

```javascript
const BASE = "https://api.example.com";
const KEY = process.env.LD_API_KEY;

const res = await fetch(BASE + "/v1/images/generations", {
  method: "POST",
  headers: { Authorization: `Bearer ${KEY}`, "Content-Type": "application/json" },
  body: JSON.stringify({
  "model": "<model>",
  "prompt": "赛博朋克风格的城市夜景，霓虹灯与雨后倒影",
  "size": "2K",
  "watermark": false
}),
});
if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
console.log(await res.json());
```

```go
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func main() {
	const base = "https://api.example.com"

	body := []byte(`{
  "model": "<model>",
  "prompt": "赛博朋克风格的城市夜景，霓虹灯与雨后倒影",
  "size": "2K",
  "watermark": false
}`)
	req, err := http.NewRequest("POST", base + "/v1/images/generations", bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("LD_API_KEY"))
	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(res.Body)
		panic(fmt.Sprintf("%d %s", res.StatusCode, msg))
	}
	out, _ := io.ReadAll(res.Body)
	fmt.Println(strings.TrimSpace(string(out)))
}
```

以上示例的请求体是「基础示例」；字段含义见第 3 节。

**图层拆分（1 张图拆成底图 + 图层）**

```bash
curl -X POST \
  "https://api.example.com/v1/images/generations" \
  -H "Authorization: Bearer $LD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "<model>",
  "image": "https://cdn.example.com/poster.png",
  "layer_decomposition": true,
  "size": "auto"
}'
```

```python
import os
import requests

BASE = "https://api.example.com"
HEADERS = {"Authorization": "Bearer " + os.environ["LD_API_KEY"]}

payload = {
    "model": "<model>",
    "image": "https://cdn.example.com/poster.png",
    "layer_decomposition": True,
    "size": "auto",
}

r = requests.post(BASE + "/v1/images/generations", headers=HEADERS, json=payload, timeout=(10, 300))
r.raise_for_status()
print(r.json())
```

## 6. 错误码

| HTTP | code | 含义 | 处理建议 |
| --- | --- | --- | --- |
| 400 | `invalid_param` | 参数不合法（字段缺失、取值越界、素材地址不可访问）。 | 对照字段表核对参数，错误信息会指出具体字段。 |
| 401 | `invalid_api_key` | KEY 无效、已过期或被禁用。 | 检查 KEY 是否完整、是否已在控制台删除或过期。 |
| 402 | `insufficient_balance` | 额度不足，请充值。 |  |
| 403 | `modality_not_allowed` | 该模态不在 KEY 的白名单里。 |  |
| 403 | `model_not_allowed` | 该 KEY 没有这个模型的权限。 | 确认这个 KEY 的可用模型包含当前模型。 |
| 404 | `model_not_found` | 模型不存在或已下线。 | 核对模型 ID 拼写；模型可能已下线，见模型广场。 |
| 413 | `request_too_large` | 请求体超过上限。 |  |
| 429 | `rate_limited` | 请求过于频繁，稍后重试。 | 按退避重试；持续触发请申请更高并发。 |
| 500 | `internal_error` | 服务端异常，本次不计费。 |  |
| 503 | `model_unavailable` | 该模型暂不可用，请更换模型或稍后重试。 |  |
| 503 | `service_unavailable` | 本次调用未能完成，未产生费用。 | 稍后重试，本次未扣费。 |

## 7. 相关接口

同属「即梦格式」的其它接口：

| 接口 | 方法与路径 | 文档 |
| --- | --- | --- |
| 图片编辑（传文件）（同一流程） | `POST /v1/images/edits` | `ep-images-edits` |
| 图片变体（同一流程） | `POST /v1/images/variations` | `ep-images-variations` |

**支持本接口的模型文档**（计费方式、档位与上限写在那边）：

| 文档 | 模型 |
| --- | --- |
| 图片生成 | `doubao-seedream-5-0-260128` |

本接口不写计费数值：单价与档位属于模型事实，见上表对应文档，避免两处口径慢慢走偏。

## 8. 注意事项

- 本篇是即梦格式原生报文，只适用于按本篇字段提交的模型；其它模型请改看OpenAI 原生报文「图片生成」（`ep-images-generations`） —— 本篇的字段它们不认识。
- 模型能力（照厂商文档）：Seedream 5.0 pro / flash 支持图层拆分与交互编辑，只出单图，不支持组图 / 联网搜索 / 流式输出；Seedream 5.0 lite / 4.5 / 4.0 支持组图与流式输出（联网搜索仅 5.0 lite），不支持图层拆分。
- 这篇是厂商原生报文：字段名、默认值、取值范围都照厂商文档原样，平台不替它做归一；平台只替换 model（对外 ID → 厂商真实模型名），其余字段原样转发。
- 平台归一的别名（resolution / image_size / aspect_ratio / ratio）与 n / quality 不属于这套报文：尺寸只认 size，张数只认 sequential_image_generation_options.max_images。
- 图层拆分按「1 张底图 + 最多 16 个图层」出图，所以该模型的张数上限必须 ≥ 17。

---

> 契约版本 2 · 接口 `images/generations` · 图像 / 即梦格式
