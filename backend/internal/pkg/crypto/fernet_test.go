package crypto

import (
	"strings"
	"testing"
)

// TestFernetRoundTrip 校验加解密可逆且 token 形态正确。
func TestFernetRoundTrip(t *testing.T) {
	master := FernetDerive("0123456789abcdef0123456789abcdef")
	plain := "sk-abcdef1234567890"

	token, err := FernetEncrypt(master, []byte(plain))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if !strings.HasPrefix(token, "gAAAAA") {
		t.Fatalf("token 前缀不符合 Fernet 体例: %s", token)
	}
	got, err := FernetDecrypt(master, token)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(got) != plain {
		t.Fatalf("明文不一致: got %q want %q", got, plain)
	}
}

// TestFernetTamper 校验密文被篡改时签名校验必须失败。
func TestFernetTamper(t *testing.T) {
	master := FernetDerive("master-key")
	token, err := FernetEncrypt(master, []byte("secret"))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	tampered := token[:len(token)-2] + "AA"
	if _, err := FernetDecrypt(master, tampered); err == nil {
		t.Fatal("篡改后的 token 不应解密成功")
	}
}

// TestFernetWrongKey 校验错误主密钥必须解密失败。
func TestFernetWrongKey(t *testing.T) {
	token, err := FernetEncrypt(FernetDerive("key-a"), []byte("secret"))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if _, err := FernetDecrypt(FernetDerive("key-b"), token); err == nil {
		t.Fatal("错误主密钥不应解密成功")
	}
}

// TestFernetKeyLength 校验主密钥长度约束。
func TestFernetKeyLength(t *testing.T) {
	if _, err := FernetEncrypt([]byte("short"), []byte("x")); err == nil {
		t.Fatal("非法长度主密钥应报错")
	}
	if _, err := FernetDecrypt([]byte("short"), "gAAAAA"); err == nil {
		t.Fatal("非法长度主密钥解密应报错")
	}
}

// TestFernetEmptyPlain 校验空明文可正常往返（PKCS7 填充满块）。
func TestFernetEmptyPlain(t *testing.T) {
	master := FernetDerive("k")
	token, err := FernetEncrypt(master, nil)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	got, err := FernetDecrypt(master, token)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空明文往返应仍为空，got %q", got)
	}
}
