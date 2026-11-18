package docdocs

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 板块（侧栏 L1）：`doc_sections` 是唯一的读取来源。
//
// 它以前是 `apidoc/section.go` 里的 Go 常量 —— 左栏第一层的名字、英文名、描述、
// 颜色、顺序都改不了。现在库里存的就是页面上显示的那一份，代码只负责**首次建库**
// 时把出厂值写进去（`SeedSections`），之后永不再改。
//
// 三条边界：
//  1. `key` 是身份（`doc_nodes.section` 指向它），创建后不可改；
//  2. 不提供新建/删除：新 key 接不到契约侧任何传输，删 key 会让其下节点失去归属；
//  3. 排序用 `sort`（间隔 10），同值按 key —— 顺序必须可解释，不能靠插入顺序。

// SectionView 是板块的对外形状（管理台与公开投影共用）。
type SectionView struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	En      string `json:"en"`
	Desc    string `json:"desc"`
	Color   string `json:"color"`
	Sort    int    `json:"sort"`
	Hidden  bool   `json:"hidden"`
	Version int    `json:"version"`
}

// SectionInput 是板块的可编辑内容（key 不在这里：它是身份）。
type SectionInput struct {
	Name    string `json:"name"`
	En      string `json:"en"`
	Desc    string `json:"desc"`
	Color   string `json:"color"`
	Sort    int    `json:"sort"`
	Hidden  bool   `json:"hidden"`
	Version int    `json:"version"`
}

// colorRE 是颜色格式。前台把它当 CSS 颜色直接下发，格式错就是一处空白。
var colorRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// SeedSections 只插缺失的板块，绝不覆盖已有行。
//
// 出厂值来自 `apidoc.Sections()`（那里保留了板块 key、顺序与初始文案），
// 它是**种子**，不是运行期真值：启动之后再改库，代码不会回写。
func SeedSections() (int, error) {
	var have []string
	if err := db.DB.Model(&model.DocSection{}).Pluck("key", &have).Error; err != nil {
		return 0, err
	}
	exists := make(map[string]bool, len(have))
	for _, k := range have {
		exists[k] = true
	}
	created := 0
	for i, s := range apidoc.Sections() {
		if exists[s.ID] {
			continue
		}
		row := model.DocSection{
			Key: s.ID, Name: s.Name, En: s.En, Desc: s.Desc, Color: s.Color,
			Sort: i * 10, Version: 1,
		}
		if err := db.DB.Create(&row).Error; err != nil {
			return created, fmt.Errorf("写入板块 %s: %w", s.ID, err)
		}
		created++
	}
	return created, nil
}

// LoadSections 返回全部板块（含隐藏），按 sort,key 排序。
//
// 不过滤 hidden：管理台要能看到并编辑隐藏板块；公开侧由调用方跳过。
func LoadSections() ([]SectionView, error) { return loadSections(db.DB) }

func loadSections(q *gorm.DB) ([]SectionView, error) {
	var rows []model.DocSection
	if err := q.Order("sort, key").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]SectionView, 0, len(rows))
	for _, r := range rows {
		out = append(out, SectionView{
			Key: r.Key, Name: r.Name, En: r.En, Desc: r.Desc, Color: r.Color,
			Sort: r.Sort, Hidden: r.Hidden, Version: r.Version,
		})
	}
	return out, nil
}

// SectionIndex 返回 key → 板块（投影、校验、可见性共用同一份读取）。
func SectionIndex() (map[string]SectionView, error) {
	list, err := LoadSections()
	if err != nil {
		return nil, err
	}
	out := make(map[string]SectionView, len(list))
	for _, s := range list {
		out[s.Key] = s
	}
	return out, nil
}

// sectionExists 判断板块 key 是否存在于库里（节点保存时校验用）。
func sectionExists(key string) bool {
	if strings.TrimSpace(key) == "" {
		return false
	}
	idx, err := SectionIndex()
	if err != nil {
		return false
	}
	_, ok := idx[key]
	return ok
}

// SaveSection 保存板块内容（key 不可改，乐观锁 version）。
func SaveSection(key string, in SectionInput, adminID uint64) (*SectionView, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, inputErrf("板块名称不能为空")
	}
	color := strings.TrimSpace(in.Color)
	if color != "" && !colorRE.MatchString(color) {
		return nil, inputErrf("颜色必须是 #RRGGBB 格式")
	}
	desc := strings.TrimSpace(in.Desc)
	en := strings.TrimSpace(in.En)

	// 文案守卫：板块名与描述直接展示给读者，与接口文档正文同一套判据。
	if probs := guardSection(key, name, en, desc); len(probs) > 0 {
		return nil, &BlockedError{Problems: probs}
	}

	var out SectionView
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		var row model.DocSection
		if err := tx.Where("key = ?", key).Take(&row).Error; err != nil {
			return err
		}
		if in.Version != 0 && in.Version != row.Version {
			return ErrDocVersion
		}
		upd := map[string]any{
			"name": name, "en": en, "desc": desc, "color": color,
			"sort": in.Sort, "hidden": in.Hidden,
			"version": row.Version + 1, "updated_by": adminID, "updated_at": time.Now(),
		}
		if err := tx.Model(&model.DocSection{}).Where("id = ?", row.ID).Updates(upd).Error; err != nil {
			return err
		}
		row.Name, row.En, row.Desc, row.Color = name, en, desc, color
		row.Sort, row.Hidden, row.Version = in.Sort, in.Hidden, row.Version+1
		out = SectionView{
			Key: row.Key, Name: row.Name, En: row.En, Desc: row.Desc, Color: row.Color,
			Sort: row.Sort, Hidden: row.Hidden, Version: row.Version,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// guardSection 扫板块的名/英文名/描述。
func guardSection(key, name, en, desc string) []Problem {
	var out []Problem
	for _, it := range []guarded{
		{path: "sections." + key + ".name", text: name},
		{path: "sections." + key + ".en", text: en},
		{path: "sections." + key + ".desc", text: desc},
	} {
		out = append(out, guardOne(it)...)
	}
	return out
}
