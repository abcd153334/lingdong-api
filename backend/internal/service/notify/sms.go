package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
)

var notifyHTTP = &http.Client{Timeout: 10 * time.Second}

// sendSMS 按当前启用渠道发送验证码短信，并计入每日限额。
func sendSMS(ctx context.Context, phone, code string) error {
	s := SMS()
	if !SMSReady() {
		return errs.NotifyChannelDisabled
	}
	if err := consumeDailyLimit(ctx, s.DailyLimit); err != nil {
		return err
	}
	ch := s.Channels[s.Active]
	switch s.Active {
	case "aliyun":
		return sendAliyun(ctx, ch, phone, code)
	case "tencent":
		return sendTencent(ctx, ch, phone, code)
	}
	return errs.NotifyChannelDisabled
}

// consumeDailyLimit 计入当日发送量（无 Redis 时降级为不限额）。
func consumeDailyLimit(ctx context.Context, limit int64) error {
	if limit <= 0 || !db.RedisReady() {
		return nil
	}
	key := "ld:sms:daily:" + time.Now().Format("20060102")
	n, err := db.RDB.Incr(ctx, key).Result()
	if err != nil {
		return nil
	}
	if n == 1 {
		db.RDB.Expire(ctx, key, 25*time.Hour)
	}
	if n > limit {
		return errs.NotifySendFailed.WithMessage("短信每日限额已用尽")
	}
	return nil
}

// sendAliyun 走阿里云短信 Dysmsapi（RPC 风格 GET + HMAC-SHA1 签名）。
func sendAliyun(ctx context.Context, ch map[string]any, phone, code string) error {
	now := time.Now().UTC()
	tp, err := marshalJSON(map[string]string{"code": code})
	if err != nil {
		return errs.NotifySendFailed
	}
	params := map[string]string{
		"AccessKeyId":      jsonString(ch, "access_key_id"),
		"Action":           "SendSms",
		"Format":           "JSON",
		"PhoneNumbers":     phone,
		"RegionId":         "cn-hangzhou",
		"SignName":         jsonString(ch, "sign_name"),
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   strconv.FormatInt(now.UnixNano(), 10),
		"SignatureVersion": "1.0",
		"TemplateCode":     jsonString(ch, "template_code"),
		"TemplateParam":    tp,
		"Timestamp":        now.Format("2006-01-02T15:04:05Z"),
		"Version":          "2017-05-25",
	}
	canonical, sig := aliyunSign(params, jsonString(ch, "access_key_secret"))
	endpoint := "https://dysmsapi.aliyuncs.com/?Signature=" + percentEncode(sig) + "&" + canonical
	body, err := doJSON(ctx, http.MethodGet, endpoint, nil, nil)
	if err != nil {
		return err
	}
	var out struct{ Code, Message string }
	if err := json.Unmarshal(body, &out); err != nil {
		return errs.NotifySendFailed.WithMessage("阿里云短信响应解析失败")
	}
	if out.Code != "OK" {
		return errs.NotifySendFailed.WithMessage("阿里云短信：" + out.Code + " " + out.Message)
	}
	return nil
}

// aliyunSign 计算 RPC 签名，返回规范化查询串（按参数名排序）与签名。
func aliyunSign(params map[string]string, secret string) (canonical, sig string) {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, percentEncode(k)+"="+percentEncode(params[k]))
	}
	canonical = strings.Join(parts, "&")
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte("GET&%2F&" + percentEncode(canonical)))
	return canonical, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// percentEncode 按阿里云 RPC 规范做百分号编码（QueryEscape 的 +/*/~ 变体修正）。
func percentEncode(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "*", "%2A")
	e = strings.ReplaceAll(e, "%7E", "~")
	return e
}

// sendTencent 走腾讯云短信 SMS API（TC3-HMAC-SHA256 + JSON POST）。
func sendTencent(ctx context.Context, ch map[string]any, phone, code string) error {
	now := time.Now().UTC()
	payload, err := json.Marshal(map[string]any{
		"PhoneNumberSet":   []string{normalizeCNPhone(phone)},
		"SmsSdkAppId":      jsonString(ch, "sdk_app_id"),
		"SignName":         jsonString(ch, "sign_name"),
		"TemplateId":       jsonString(ch, "template_id"),
		"TemplateParamSet": []string{code},
	})
	if err != nil {
		return errs.NotifySendFailed
	}
	host := "sms.tencentcloudapi.com"
	ts := strconv.FormatInt(now.Unix(), 10)
	headers := map[string]string{
		"Authorization":  tencentAuth(jsonString(ch, "secret_id"), jsonString(ch, "secret_key"), host, now.Format("2006-01-02"), ts, string(payload)),
		"Content-Type":   "application/json; charset=utf-8",
		"Host":           host,
		"X-TC-Action":    "SendSms",
		"X-TC-Timestamp": ts,
		"X-TC-Version":   "2021-01-11",
		"X-TC-Region":    "ap-guangzhou",
	}
	body, err := doJSON(ctx, http.MethodPost, "https://"+host+"/", payload, headers)
	if err != nil {
		return err
	}
	var out struct {
		Response struct {
			Error *struct{ Code, Message string }
		}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return errs.NotifySendFailed.WithMessage("腾讯云短信响应解析失败")
	}
	if e := out.Response.Error; e != nil {
		return errs.NotifySendFailed.WithMessage("腾讯云短信：" + e.Code + " " + e.Message)
	}
	return nil
}

// normalizeCNPhone 归一为 +86 前缀（腾讯云要求 E.164）。
func normalizeCNPhone(phone string) string {
	p := strings.TrimSpace(phone)
	if strings.HasPrefix(p, "+") {
		return p
	}
	return "+86" + p
}

// tencentAuth 生成 TC3-HMAC-SHA256 Authorization 头。
func tencentAuth(secretID, secretKey, host, date, ts, payload string) string {
	scope := date + "/sms/tc3_request"
	canonicalHeaders := "content-type:application/json; charset=utf-8\nhost:" + host + "\n"
	signedHeaders := "content-type;host"
	canonicalRequest := strings.Join([]string{
		"POST", "/", "", canonicalHeaders, signedHeaders, sha256Hex(payload),
	}, "\n")
	stringToSign := strings.Join([]string{
		"TC3-HMAC-SHA256", ts, scope, sha256Hex(canonicalRequest),
	}, "\n")
	secretDate := hmacSHA256([]byte("TC3"+secretKey), date)
	secretService := hmacSHA256(secretDate, "sms")
	secretSigning := hmacSHA256(secretService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))
	return "TC3-HMAC-SHA256 Credential=" + secretID + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// doJSON 执行 HTTP 请求并读取响应体（限额 64KB）。
func doJSON(ctx context.Context, method, endpoint string, payload []byte, headers map[string]string) ([]byte, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, errs.NotifySendFailed
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := notifyHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errs.NotifySendFailed, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, errs.NotifySendFailed
	}
	if resp.StatusCode >= 500 {
		return nil, errs.NotifySendFailed.WithMessage("短信网关返回 " + resp.Status)
	}
	return body, nil
}
