package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"lingdong-api/internal/pkg/errs"
)

// sendMail 通过 SMTP 发送一封纯文本邮件；encryption ∈ ssl / starttls / none。
func sendMail(ctx context.Context, e EmailSettings, to, subject, body string) error {
	addr := net.JoinHostPort(e.SMTPHost, strconv.Itoa(e.SMTPPort))
	client, err := dialSMTP(ctx, addr, e)
	if err != nil {
		return fmt.Errorf("%w: %v", errs.NotifySendFailed, err)
	}
	defer client.Close()

	if e.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", e.Username, e.Password, e.SMTPHost)); err != nil {
			return fmt.Errorf("%w: 认证失败: %v", errs.NotifySendFailed, err)
		}
	}
	if err := client.Mail(e.FromAddr); err != nil {
		return fmt.Errorf("%w: %v", errs.NotifySendFailed, err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("%w: %v", errs.NotifySendFailed, err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("%w: %v", errs.NotifySendFailed, err)
	}
	if _, err := w.Write([]byte(buildMessage(e, to, subject, body))); err != nil {
		return fmt.Errorf("%w: %v", errs.NotifySendFailed, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("%w: %v", errs.NotifySendFailed, err)
	}
	return client.Quit()
}

// dialSMTP 按加密方式建立连接（ssl = 隐式 TLS，starttls = 明文后升级，none = 明文）。
func dialSMTP(ctx context.Context, addr string, e EmailSettings) (*smtp.Client, error) {
	if e.Encryption == "ssl" {
		conn, err := (&tls.Dialer{Config: &tls.Config{ServerName: e.SMTPHost}}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		return smtp.NewClient(conn, e.SMTPHost)
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	client, err := smtp.NewClient(conn, e.SMTPHost)
	if err != nil {
		return nil, err
	}
	if e.Encryption == "starttls" {
		if err := client.StartTLS(&tls.Config{ServerName: e.SMTPHost}); err != nil {
			client.Close()
			return nil, err
		}
	}
	return client, nil
}

// buildMessage 组装 RFC 5322 报文：主题 RFC2047 编码，正文 UTF-8 + base64 传输编码。
func buildMessage(e EmailSettings, to, subject, body string) string {
	var b strings.Builder
	from := e.FromAddr
	if e.SenderName != "" {
		from = mime.QEncoding.Encode("UTF-8", e.SenderName) + " <" + e.FromAddr + ">"
	}
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("UTF-8", subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString(wrapBase64(body))
	return b.String()
}

// wrapBase64 将 UTF-8 正文 base64 编码并按 76 列折行（RFC 2045）。
func wrapBase64(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteString("\r\n")
	return b.String()
}
