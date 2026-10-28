package loomy

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"math/big"
	"strings"
	"testing"
)

// 移植自 tests/test_crypto.py：DER 解析 + RSA/PKCS#1 v1.5 加解密往返。

// 账号服务线上下发的公钥（公开数据，不含秘密），原样保留。
const realPukey = "MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDNCA0OSvscqyTGPguV7roG6Ct6jqpRVuL1" +
	"RtroTH97JBizHdLUaF5Fz9MyKmeFXPVtFn/CINsLzSqxuA8uT4cR5fKmsKlBqAyHNt+sSE+B" +
	"6DUDspT5zFPDc/3IeI3/A9sTsl5n1XOLAPjyBodbG2FcDPWp9+MaFoMjaUDvDQ8BJQIDAQAB"

// 测试用 512 位 RSA 密钥（由 Python tests/support.py 的 rsa_keypair(512) 生成，
// 仅用于本地往返，不含任何真实凭据）。
//
//	n.bit_length() == 511 → k == 64 字节。
const (
	testRSANHex = "7962c20698b8f41da4fbefd166cf02b805822cb9de96da9e992e6a29f9283d96" +
		"e670f1af8045812f047a34404c58f114adb4f57814368cb5dfe4f565be450b97"
	testRSADHex = "310f714c293dd7ac839a2ffb7a8a00974fe2a5211a8dee1e5bdb1e9d884e735" +
		"5916e660551a5bd6613e32f365b6ba20e9c3f2f2dcf96c9c2fb69814782c76bc1"
	testRSAE = 65537
)

// ------------------------------------------------------------ DER test helpers

func testDERLen(length int) []byte {
	if length < 0x80 {
		return []byte{byte(length)}
	}
	var raw []byte
	for v := length; v > 0; v >>= 8 {
		raw = append([]byte{byte(v)}, raw...)
	}
	return append([]byte{0x80 | byte(len(raw))}, raw...)
}

func testDERWrap(tag byte, content []byte) []byte {
	out := []byte{tag}
	out = append(out, testDERLen(len(content))...)
	return append(out, content...)
}

func testDERInt(v *big.Int) []byte {
	raw := v.Bytes()
	if len(raw) == 0 {
		raw = []byte{0}
	}
	if raw[0]&0x80 != 0 { // 保证 INTEGER 为正
		raw = append([]byte{0x00}, raw...)
	}
	return testDERWrap(0x02, raw)
}

// testSPKIDERB64 对应 tests/support.py 的 spki_der_b64。
func testSPKIDERB64(n *big.Int, e int) string {
	rsaOID := []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x01}
	alg := testDERWrap(0x30, append(append([]byte{}, rsaOID...), 0x05, 0x00))
	key := testDERWrap(0x30, append(testDERInt(n), testDERInt(big.NewInt(int64(e)))...))
	spki := testDERWrap(0x30, append(alg, testDERWrap(0x03, append([]byte{0x00}, key...))...))
	return base64.StdEncoding.EncodeToString(spki)
}

func testRSAKey(t *testing.T) (n *big.Int, d *big.Int) {
	t.Helper()
	n, ok := new(big.Int).SetString(testRSANHex, 16)
	if !ok {
		t.Fatal("bad test modulus literal")
	}
	d, ok = new(big.Int).SetString(testRSADHex, 16)
	if !ok {
		t.Fatal("bad test exponent literal")
	}
	return n, d
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode %q: %v", s, err)
	}
	return out
}

// ------------------------------------------------------------------------- DER

func TestDerRealPublicKey(t *testing.T) {
	n, e, err := DerToNumbers(mustB64(t, realPukey))
	if err != nil {
		t.Fatalf("DerToNumbers: %v", err)
	}
	if e.Int64() != testRSAE {
		t.Fatalf("exponent = %s, want %d", e, testRSAE)
	}
	if n.BitLen() != 1024 {
		t.Fatalf("modulus bits = %d, want 1024", n.BitLen())
	}
}

func TestDerGeneratedKeyRoundTrip(t *testing.T) {
	n, _ := testRSAKey(t)
	n2, e2, err := DerToNumbers(mustB64(t, testSPKIDERB64(n, testRSAE)))
	if err != nil {
		t.Fatalf("DerToNumbers: %v", err)
	}
	if n2.Cmp(n) != 0 || e2.Int64() != testRSAE {
		t.Fatalf("round trip mismatch: n=%s e=%s", n2, e2)
	}
}

func TestDerRejectsGarbage(t *testing.T) {
	if _, _, err := DerToNumbers([]byte{0x31, 0x02, 0x00, 0x00}); err == nil {
		t.Fatal("expected an error for a non-SEQUENCE DER blob")
	}
}

// -------------------------------------------------------------- RSA encryption

func TestRsaEncryptPaddingDecryptsBackToPlaintext(t *testing.T) {
	n, d := testRSAKey(t)
	pukey := testSPKIDERB64(n, testRSAE)
	k := (n.BitLen() + 7) / 8

	out, err := RsaEncryptPKCS1v15(pukey, "hunter2", nil)
	if err != nil {
		t.Fatalf("RsaEncryptPKCS1v15: %v", err)
	}
	ciphertext := mustB64(t, out)
	if len(ciphertext) != k {
		t.Fatalf("ciphertext length = %d, want %d", len(ciphertext), k)
	}

	block := new(big.Int).Exp(new(big.Int).SetBytes(ciphertext), d, n).Bytes()
	padded := make([]byte, k)
	copy(padded[k-len(block):], block)

	if !bytes.Equal(padded[:2], []byte{0x00, 0x02}) {
		t.Fatalf("block prefix = %x, want 0002", padded[:2])
	}
	sep := bytes.IndexByte(padded[2:], 0x00)
	if sep < 0 {
		t.Fatal("no 0x00 separator in padding")
	}
	sep += 2
	if !bytes.Equal(padded[sep+1:], []byte("hunter2")) {
		t.Fatalf("plaintext = %q, want %q", padded[sep+1:], "hunter2")
	}
	if sep-2 < 8 { // PKCS#1 要求至少 8 字节非零填充
		t.Fatalf("padding length = %d, want >= 8", sep-2)
	}
	if bytes.IndexByte(padded[2:sep], 0x00) >= 0 {
		t.Fatal("padding contains a zero byte")
	}
}

func TestRsaEncryptDeterministicPaddingForTests(t *testing.T) {
	n, d := testRSAKey(t)
	pukey := testSPKIDERB64(n, testRSAE)
	k := (n.BitLen() + 7) / 8
	size := k - len("x") - 3

	out, err := RsaEncryptPKCS1v15(pukey, "x", bytes.NewReader(bytes.Repeat([]byte{0x41}, size)))
	if err != nil {
		t.Fatalf("RsaEncryptPKCS1v15: %v", err)
	}
	ciphertext := mustB64(t, out)
	if len(ciphertext) != k {
		t.Fatalf("ciphertext length = %d, want %d", len(ciphertext), k)
	}

	block := make([]byte, k)
	raw := new(big.Int).Exp(new(big.Int).SetBytes(ciphertext), d, n).Bytes()
	copy(block[k-len(raw):], raw)
	if want := bytes.Repeat([]byte{0x41}, size); !bytes.Equal(block[2:2+size], want) {
		t.Fatal("injected padding bytes did not reach the PKCS#1 block")
	}
}

// rng 里出现 0x00 时必须重读（PKCS#1 填充字节不能为零）。
func TestRsaEncryptSkipsZeroPadding(t *testing.T) {
	n, d := testRSAKey(t)
	pukey := testSPKIDERB64(n, testRSAE)
	k := (n.BitLen() + 7) / 8
	size := k - len("x") - 3

	rng := bytes.NewReader(append(bytes.Repeat([]byte{0x00}, size), bytes.Repeat([]byte{0x42}, size)...))
	out, err := RsaEncryptPKCS1v15(pukey, "x", rng)
	if err != nil {
		t.Fatalf("RsaEncryptPKCS1v15: %v", err)
	}

	block := make([]byte, k)
	raw := new(big.Int).Exp(new(big.Int).SetBytes(mustB64(t, out)), d, n).Bytes()
	copy(block[k-len(raw):], raw)
	if want := bytes.Repeat([]byte{0x42}, size); !bytes.Equal(block[2:2+size], want) {
		t.Fatal("zero-filled padding attempt was not retried")
	}
}

func TestRsaEncryptMessageTooLong(t *testing.T) {
	n, _ := testRSAKey(t)
	_, err := RsaEncryptPKCS1v15(testSPKIDERB64(n, testRSAE), strings.Repeat("x", 200), nil)
	if err == nil {
		t.Fatal("expected an error for an over-long message")
	}
	if want := "message too long for RSA-512"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err, want)
	}
}

// ------------------------------------------------------------------- env 解析

func TestParseEnv(t *testing.T) {
	got := ParseEnv("# c\nA=1\nB = two \n\n")
	want := map[string]string{"A": "1", "B": "two"}
	if len(got) != len(want) {
		t.Fatalf("ParseEnv = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("ParseEnv[%q] = %q, want %q", k, got[k], v)
		}
	}

	// \r\n（客户端 .env.prod 常见）同样按行切分。
	if crlf := ParseEnv("A=1\r\nB=two\r\n"); crlf["A"] != "1" || crlf["B"] != "two" {
		t.Fatalf("CRLF ParseEnv = %v", crlf)
	}
}

func TestDecryptClientEnvPlaintextPassthrough(t *testing.T) {
	got, err := DecryptClientEnv("A=1")
	if err != nil {
		t.Fatalf("DecryptClientEnv: %v", err)
	}
	if got != "A=1" {
		t.Fatalf("DecryptClientEnv = %q, want %q", got, "A=1")
	}
}

func TestDecryptClientEnvPaddingLenientBase64(t *testing.T) {
	// 客户端密文的 base64 经常缺 padding；解密助手不能在这里就炸掉，
	// 而应该走到 AES-GCM 校验并失败（这里密文是垃圾）。
	raw := EnvCryptMagic + ":" +
		strings.TrimRight(base64.StdEncoding.EncodeToString(make([]byte, 47)), "=")
	if _, err := DecryptClientEnv(raw); err == nil {
		t.Fatal("expected a decryption error for a garbage payload")
	}
}

func TestDecryptClientEnvRoundTrip(t *testing.T) {
	const want = "BASE_URL=https://example.invalid/api/v1\n# 注释\nUSER=loomy\n"

	salt := []byte("0123456789abcdef")
	iv := []byte("abcdefghijkl")

	key, err := Scrypt([]byte(EnvCryptPassphrase), salt, 16384, 8, 1, KeyLen)
	if err != nil {
		t.Fatalf("Scrypt: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	sealed := gcm.Seal(nil, iv, []byte(want), nil)
	data := sealed[:len(sealed)-TagLen]
	tag := sealed[len(sealed)-TagLen:]

	payload := make([]byte, 0, SaltLen+IVLen+TagLen+len(data))
	payload = append(payload, salt...)
	payload = append(payload, iv...)
	payload = append(payload, tag...)
	payload = append(payload, data...)

	// 去掉 base64 padding，顺便覆盖客户端的缺 padding 情况。
	raw := EnvCryptMagic + ":" + strings.TrimRight(base64.StdEncoding.EncodeToString(payload), "=")
	got, err := DecryptClientEnv(raw)
	if err != nil {
		t.Fatalf("DecryptClientEnv: %v", err)
	}
	if got != want {
		t.Fatalf("DecryptClientEnv = %q, want %q", got, want)
	}
}
