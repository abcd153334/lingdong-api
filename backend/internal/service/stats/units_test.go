package stats

import (
	"reflect"
	"strings"
	"testing"
)

// TestContractHasNoCentField 守「统计契约只有一种金额量纲：微」。
//
// 为什么用反射守而不是靠 code review：
// `recharge_cent` 这种字段名自带量纲提示，看起来"很清楚"，但它把换算责任
// 推给了每一个消费方。前端三处用到充值（今日卡片、趋势图、毛利）各写一次
// 换算，漏一处的表现是「毛利 = 成本的一万分之一」—— 接口 200、页面不报错、
// 数字还挺像真的，只有对账时才发现。
//
// 所以规则写死：**本包对外结构里不允许出现 Cent / Minor 语义的字段**。
// 订单表仍然存分（支付渠道只认分），但那只在包内部，出口必须经 microOfCents。
func TestContractHasNoCentField(t *testing.T) {
	for _, v := range []any{
		Board{}, TodayView{}, TrendPoint{}, RankView{},
		RangeView{}, UserAggView{}, ChannelView{},
		UserOverviewView{}, UserUsageStat{}, UserTrendPoint{},
	} {
		assertNoMinorUnit(t, reflect.TypeOf(v), reflect.TypeOf(v).Name())
	}
}

// assertNoMinorUnit 递归检查结构体（含嵌套/切片元素）里没有「分」量纲的字段。
func assertNoMinorUnit(t *testing.T, typ reflect.Type, path string) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		key := f.Tag.Get("json")
		if k, _, ok := strings.Cut(key, ","); ok {
			key = k
		}
		// 按「词」而不是按子串判断：`percent` 里含 cent，但它不是分。
		if tok := minorToken(strings.ToLower(key)); tok != "" {
			t.Fatalf("%s.%s（json=%q）是「%s」量纲字段：统计接口只允许下发微，"+
				"订单的分请在出口用 microOfCents 换算", path, f.Name, key, tok)
		}
		if tok := minorToken(strings.ToLower(f.Name)); tok != "" {
			t.Fatalf("%s.%s 字段名声明了「%s」量纲：统计接口只允许下发微，"+
				"订单的分请在出口用 microOfCents 换算", path, f.Name, tok)
		}
		assertNoMinorUnit(t, f.Type, path+"."+f.Name)
	}
}

// minorToken 判断一个标识符（snake_case 或 Go camelCase 小写化后）是否以
// 非微的量纲词结尾/分词，返回命中的词；不是则返回空串。
func minorToken(ident string) string {
	for _, tok := range strings.Split(ident, "_") {
		switch tok {
		case "cent", "cents", "minor", "yuan", "fen":
			return tok
		}
	}
	// camelCase 没有下划线，按后缀判断。
	for _, tok := range []string{"cents", "cent", "minor", "yuan", "fen"} {
		if strings.HasSuffix(ident, tok) && !strings.HasSuffix(ident, "percent") {
			return tok
		}
	}
	return ""
}
