package storage

import (
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"lingdong-api/internal/pkg/netguard"
)

// TestIdleTimeoutBodyFires 单次读超时必须返回 ErrObjectReadTimeout。
//
// 这是 P2-15 的核心：改造前用 http.Client.Timeout=60s 卡大对象，超时是**整体取消**，
// 已收的字节全废；现在只判「单次读」超时，慢但在传的连接不会被误杀。
func TestIdleTimeoutBodyFires(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	body := newIdleTimeoutBody(pr, 50*time.Millisecond)
	start := time.Now()
	_, err := body.Read(make([]byte, 8))
	elapsed := time.Since(start)

	if !errors.Is(err, ErrObjectReadTimeout) {
		t.Fatalf("期望 ErrObjectReadTimeout，实际 %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("超时未生效：阻塞了 %s", elapsed)
	}
	_ = body.Close()
}

// TestIdleTimeoutBodyAllowsSlowButSteady 每次读都有进展时不得触发超时。
func TestIdleTimeoutBodyAllowsSlowButSteady(t *testing.T) {
	pr, pw := io.Pipe()
	body := newIdleTimeoutBody(pr, 80*time.Millisecond)

	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(25 * time.Millisecond)
			if _, err := pw.Write([]byte("a")); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()

	total := 0
	buf := make([]byte, 4)
	for {
		n, err := body.Read(buf)
		total += n
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("慢但持续的读被误判超时: %v（已读 %d 字节）", err, total)
		}
	}
	if total != 5 {
		t.Fatalf("读到的字节数不符: %d", total)
	}
	_ = body.Close()
}

// TestGuardedClientTimeoutProfile 校验「不设整体超时 + 共享连接池 + 三个时间常量」的配置形状。
//
// 断言字面量而不是行为：这组值是容量规划的一部分（512MB @ 300KB/s ≈ 29min），
// 被无意改小时大文件会重新变成「永远转存失败」，而这种回归在单测里没有别的抓法。
//
// 最要命的一条是 `Timeout == 0`：给 http.Client 设整体超时（曾经的 60s）会把
// 「慢但在持续传输」的大文件整段作废，重试仍会在同一处失败。
func TestGuardedClientTimeoutProfile(t *testing.T) {
	c := guardedClient()
	if c.Timeout != 0 {
		t.Fatalf("不得设置 http.Client.Timeout（会截断大文件），实际 %s", c.Timeout)
	}
	// 三个驱动必须共用同一份连接池：各建一份等于每个驱动各自 TLS 握手，且 netguard 策略漂移。
	if c.Transport != netguard.Transport(netguard.PolicyFromEnv()) {
		t.Fatal("Transport 必须复用 netguard 的进程级共享连接池")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transporter 类型不符: %T", c.Transport)
	}
	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Fatalf("TLS 握手超时应为 10s，实际 %s", tr.TLSHandshakeTimeout)
	}
	if tr.DialContext == nil {
		t.Fatal("缺少 DialContext（等于回到系统默认的无限等待，且绕过 netguard 的 IP 校验）")
	}
	if c.CheckRedirect == nil {
		t.Fatal("必须显式禁止重定向（运营可配 endpoint 会变成 SSRF 通道）")
	}

	if dataOpTimeout != 30*time.Minute {
		t.Fatalf("数据面整体超时应为 30min，实际 %s", dataOpTimeout)
	}
	if metaOpTimeout != 60*time.Second {
		t.Fatalf("元数据面整体超时应为 60s，实际 %s", metaOpTimeout)
	}
	if readIdleTimeout != 60*time.Second {
		t.Fatalf("单次读空闲超时应为 60s，实际 %s", readIdleTimeout)
	}
}
