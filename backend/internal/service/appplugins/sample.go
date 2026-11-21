package appplugins

// SampleManifest 是给管理员的**出厂模板**，也是给 AI 的 few-shot 示例。
//
// 它只通过 GET /api/admin/app-plugins/sample 下发，**不写库**：
// 落库会变成"运营删了、重启又回来"，而 v7_seed 的既定规则是"只插不改"——
// 一个删不掉的示例比没有示例更让人困惑。
//
// 约束（示例会原样出现在 docs/应用插件开发手册.md 的两个 json 块里，
// 由 sample_test 与 devdoc 的 golden 用例逐字节比对）：
//   - 每个声明字段都在示例里被用到（否则就是"声明了没人读"）；
//   - icon 留空：内置图标目录里全是厂商 logo，随意挑一个比留空更误导；
//     要图标就先用图标库上传，再填 upload:<id>；
//   - 只覆盖基座真有的能力：string / enum / text / date 四种类型，
//     create/update/delete/export 四个动作；
//   - public 演示白名单：只挑"给外面看也无所谓"的两列（标题、状态），
//     单号与详情不公开 —— 白名单的意义就是逐列决定，不是整对象开关；
//   - 不出现 money、file、webhook、脚本 —— 这些基座不支持，示例里更不能出现。
const SampleManifest = `{
  "apiVersion": 1,
  "key": "ticket",
  "name": "工单",
  "version": "1.0.0",
  "description": "接入方提交的问题与处理进度。",
  "icon": "",
  "config": [
    { "key": "slaHours", "label": "响应时限（小时）", "type": "int", "required": true, "default": 24 }
  ],
  "entities": [
    {
      "key": "ticket",
      "name": "工单",
      "unique": "ticketNo",
      "fields": [
        { "key": "ticketNo", "label": "单号", "type": "string", "required": true, "maxLen": 32 },
        { "key": "title", "label": "标题", "type": "string", "required": true, "maxLen": 80 },
        { "key": "status", "label": "状态", "type": "enum", "required": true, "options": ["open", "doing", "done"], "default": "open" },
        { "key": "due", "label": "期望完成", "type": "date" },
        { "key": "detail", "label": "详情", "type": "text", "maxLen": 2000 }
      ],
      "list": {
        "columns": ["ticketNo", "title", "status", "due"],
        "actions": ["create", "update", "delete", "export"]
      },
      "public": ["title", "status"]
    }
  ],
  "limits": { "maxRecords": 20000 }
}`
