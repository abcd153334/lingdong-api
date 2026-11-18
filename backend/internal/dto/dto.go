// Package dto 存放通用请求/响应 DTO（占位，S1+ 填充业务 DTO）。
package dto

// PageQuery 是 T21 分页查询参数。
type PageQuery struct {
	Page     int    `form:"page"`
	PageSize int    `form:"page_size"`
	Sort     string `form:"sort"`
}

// IDParam 是路径中的资源 ID 参数（占位）。
type IDParam struct {
	ID string `uri:"id" binding:"required"`
}
