package controller

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/uploadcfg"
	"lingdong-api/internal/service/upload"
)

// 头像上传的请求体门 = 头像上限 + 表单额外开销。
//
// 为什么不用 uploadcfg.RequestCapCached()：那是「所有上传类共享的外门」，等于把
// 头像上限绑在运营可调的图片上限上 —— 运营把图片上限调到 0.5MB，1MB 的头像就传不上，
// 报错还是「文件超过单文件上限」。图标同理（icon.go 用自己的 MaxBytes）。
const avatarBodyCap = upload.AvatarMaxBytes + uploadcfg.UploadOverhead

// AvatarUpload 处理 POST /api/portal/me/avatar（multipart，字段名 file）。
//
// 头像**不落对象存储**：编码成 data URL 直接写 users.avatar，换头像就是覆盖同一个标量，
// 没有孤儿对象、没有删除复核、也不依赖存储后端是否配置好。
func AvatarUpload(c *gin.Context) {
	uid := c.GetUint64("user_id")
	if uid == 0 {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	// MaxBytesReader 在读流时截断：只看 fh.Size 会被伪造的 Content-Length 绕过，
	// 大文件会先把内存吃满再被拒。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, avatarBodyCap)
	fh, err := c.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			response.FailDefMsg(c, errs.FileTooLarge, "头像不能超过 2MB")
			return
		}
		response.FailDef(c, errs.FileMissing)
		return
	}
	if fh.Size > upload.AvatarMaxBytes {
		response.FailDefMsg(c, errs.FileTooLarge, "头像不能超过 2MB")
		return
	}
	f, err := fh.Open()
	if err != nil {
		response.FailDef(c, errs.FileMissing)
		return
	}
	defer f.Close()

	// 多读 1 字节用于判超限：fh.Size 是 multipart 头里的声明值，不可信。
	raw, err := io.ReadAll(io.LimitReader(f, upload.AvatarMaxBytes+1))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if len(raw) > upload.AvatarMaxBytes {
		response.FailDefMsg(c, errs.FileTooLarge, "头像不能超过 2MB")
		return
	}

	dataURL, err := upload.EncodeAvatar(raw)
	if err != nil {
		response.FailDef(c, avatarFailDefOf(err))
		return
	}
	if err := db.DB.Model(&model.User{}).Where("id = ?", uid).Update("avatar", dataURL).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{"avatar_url": dataURL})
}

// AvatarDelete 处理 DELETE /api/portal/me/avatar：清空头像（前端回落默认头像）。
//
// 返回 200 + `{avatar_url: ""}` 而不是 204：前端的 `parseResponse` 没有 204 分支，
// 204 会被判成「HTTP 204」错误 —— 删除成功却弹失败提示。
func AvatarDelete(c *gin.Context) {
	uid := c.GetUint64("user_id")
	if uid == 0 {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	if err := db.DB.Model(&model.User{}).Where("id = ?", uid).Update("avatar", "").Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{"avatar_url": ""})
}

// avatarFailDefOf 把编码域哨兵映射为对外错误码。
//
// 文案固定「头像口径」：用户看到「不支持的文件类型」并不知道该传什么，
// 而「头像只支持 PNG / JPEG / WebP」可以直接照做。
func avatarFailDefOf(err error) errs.Def {
	switch {
	case errors.Is(err, upload.ErrAvatarType):
		return errs.FileTypeUnsupported.WithMessage("头像只支持 PNG / JPEG / WebP")
	case errors.Is(err, upload.ErrAvatarEmpty):
		return errs.FileEmpty.WithMessage("请选择一个图片文件")
	case errors.Is(err, upload.ErrAvatarPixels):
		return errs.FileTooLarge.WithMessage("图片尺寸过大，请先缩小")
	case errors.Is(err, upload.ErrAvatarBroken):
		return errs.InvalidParam.WithMessage("图片解析失败，请换一张")
	case errors.Is(err, upload.ErrAvatarTooBig):
		return errs.InvalidParam.WithMessage("图片仍过大，请换一张")
	default:
		return errs.Internal
	}
}
