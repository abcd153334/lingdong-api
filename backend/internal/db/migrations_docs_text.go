package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
)

// M48：字段说明从「三列」合并成「一列」。
//
// 背景：`doc_endpoint_fields` 曾经有 `desc`（说明）、`note`（补充说明）、
// `groups`（按模型分组的取值参考表）三列。页面上读者看到的是**一段连续的说明**，
// 而在管理台只有 `desc` 有输入框 —— `note` 与 `groups` 里的文字（gpt-image 的
// 15 行像素表、quality 的三档说明）后台根本看不见，于是所有人都以为那是写死在代码里的。
//
// 这一步把三列合成 `desc` 一段多行文字，然后删掉另两列。合并规则与页面的阅读顺序一致：
//
//	desc → （空行）note → （空行）groups 的文字与表格行
//
// **必须先合并再删列**，且整件事在一个事务里：中途失败留下「列删了、文字没搬」
// 就是一次内容丢失，而它不会报任何错。
func MigrateDocsFieldText(gdb *gorm.DB) (int, error) {
	hasNote, err := columnExists(gdb, "doc_endpoint_fields", "note")
	if err != nil {
		return 0, err
	}
	hasGroups, err := columnExists(gdb, "doc_endpoint_fields", "groups")
	if err != nil {
		return 0, err
	}
	if !hasNote && !hasGroups {
		return 0, nil
	}

	type fieldRow struct {
		ID     uint64
		Desc   string
		Note   string
		Groups []byte
	}
	selectSQL := `SELECT id, "desc"`
	if hasNote {
		selectSQL += `, note`
	} else {
		selectSQL += `, '' AS note`
	}
	if hasGroups {
		selectSQL += `, groups`
	} else {
		selectSQL += `, '[]'::jsonb AS groups`
	}
	selectSQL += ` FROM doc_endpoint_fields`

	var rows []fieldRow
	if err := gdb.Raw(selectSQL).Scan(&rows).Error; err != nil {
		return 0, fmt.Errorf("db: 读取字段说明失败: %w", err)
	}

	changed := 0
	err = gdb.Transaction(func(tx *gorm.DB) error {
		for _, r := range rows {
			merged := mergeFieldText(r.Desc, r.Note, r.Groups)
			if merged == r.Desc {
				continue
			}
			if err := tx.Exec(`UPDATE doc_endpoint_fields SET "desc" = ? WHERE id = ?`, merged, r.ID).Error; err != nil {
				return fmt.Errorf("db: 合并字段说明失败(id=%d): %w", r.ID, err)
			}
			changed++
		}
		for _, col := range []string{"note", "groups"} {
			ok, err := columnExists(tx, "doc_endpoint_fields", col)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := tx.Exec(`ALTER TABLE doc_endpoint_fields DROP COLUMN ` + col).Error; err != nil {
				return fmt.Errorf("db: 删除 doc_endpoint_fields.%s 失败: %w", col, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}

// mergeFieldText 按页面阅读顺序拼成一段多行文字；已合并过的行原样返回。
func mergeFieldText(desc, note string, groups []byte) string {
	parts := make([]string, 0, 3)
	if s := strings.TrimSpace(desc); s != "" {
		parts = append(parts, s)
	}
	gtext := groupsText(groups)
	if s := strings.TrimSpace(note); s != "" && s != strings.TrimSpace(desc) {
		// 同一件事的两种写法只留一份：图片 size 的 note 与分组说明都以
		// 「比例写法（如 …）」开头，两段并排读起来像重复了一遍。
		if gtext == "" || !sameTopic(s, gtext) {
			parts = append(parts, s)
		}
	}
	if gtext != "" {
		parts = append(parts, gtext)
	}
	return strings.Join(parts, "\n")
}

// sameTopic 判断两段话是不是在讲同一件事（开头一致）。
//
// 合并期只做这一种判断：**开头六个字相同**就算同一段。它足够区分真实的两种情况 ——
// size 的 note 与分组说明都以「比例写法（如」开头（同一句的两种写法），
// 而 quality 的 note 讲「不写按模型默认」、分组讲「只支持 auto」（两件事，都要留）。
func sameTopic(a, b string) bool {
	ra, rb := []rune(strings.TrimSpace(a)), []rune(strings.TrimSpace(b))
	const n = 6
	if len(ra) < n || len(rb) < n {
		return false
	}
	return string(ra[:n]) == string(rb[:n])
}

// groupsText 把 `[{models,text,rows:[{k,v}]}]` 展开成人读的几行。
//
// 有模型分组时写成「模型A、模型B：那句话」，与页面上「组头 + 正文」的读法一致；
// 参考表每行写成「左列　右列」，用全角空格对齐（等宽字体下就是一张表）。
func groupsText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var groups []struct {
		Models []string `json:"models"`
		Text   string   `json:"text"`
		Rows   []struct {
			K string `json:"k"`
			V string `json:"v"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &groups); err != nil {
		return ""
	}
	var out []string
	for _, g := range groups {
		text := strings.TrimSpace(g.Text)
		if text != "" {
			if len(g.Models) > 0 {
				out = append(out, strings.Join(g.Models, "、")+"："+text)
			} else {
				out = append(out, text)
			}
		}
		for _, r := range g.Rows {
			k, v := strings.TrimSpace(r.K), strings.TrimSpace(r.V)
			if k == "" && v == "" {
				continue
			}
			out = append(out, k+"　"+v)
		}
	}
	return strings.Join(out, "\n")
}

// LogDocsFieldText 打一行说明（合并只跑一次，出事后的唯一线索就是它）。
func LogDocsFieldText(changed int) {
	if changed == 0 {
		return
	}
	logger.L().Info("字段说明已合并为一列（desc），note / groups 两列已删除",
		zap.Int("rows", changed), zap.String("at", time.Now().Format(time.RFC3339)))
}
