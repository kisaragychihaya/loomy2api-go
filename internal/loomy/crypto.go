package loomy

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"unicode/utf8"
)

// 对应 Python loomy2api/crypto.py —— 纯标准库实现的两件事：
//
//  1. RSA/PKCS#1 v1.5 加密账号密码：用 pow() 手工实现，不依赖第三方库。
//     账号服务下发的是 1024 位 DER 公钥，bignum 模幂非常快。
//  2. 解密客户端自带的 .env.prod（AES-256-GCM + scrypt）。Python 侧是可选依赖，
//     Go 侧 scrypt/AES-GCM 都在标准库里，故始终可用。

// cryptoReadDER 对应 Python _read_der：返回 (tag, 内容起点, 内容长度)。
// 支持 0x80 长格式（长度字节数 n ∈ 1..4）。
func cryptoReadDER(buf []byte, pos int) (tag byte, body int, length int, err error) {
	if pos < 0 || pos >= len(buf) {
		return 0, 0, 0, errors.New("truncated DER")
	}
	tag = buf[pos]
	pos++
	if pos >= len(buf) {
		return 0, 0, 0, errors.New("truncated DER")
	}
	length = int(buf[pos])
	pos++
	if length&0x80 != 0 {
		n := length & 0x7f
		if n == 0 || n > 4 {
			return 0, 0, 0, errors.New("unsupported DER length")
		}
		if pos+n > len(buf) {
			return 0, 0, 0, errors.New("truncated DER")
		}
		length = 0
		for _, b := range buf[pos : pos+n] {
			length = length<<8 | int(b)
		}
		pos += n
	}
	return tag, pos, length, nil
}

// DerToNumbers 从 DER SubjectPublicKeyInfo 里取出 (modulus, exponent)。
// 处理标准布局 SEQUENCE{ SEQUENCE{ OID, NULL }, BIT STRING{
// SEQUENCE{ INTEGER n, INTEGER e } } }，不需要 ASN.1 库。
func DerToNumbers(der []byte) (n, e *big.Int, err error) {
	tag, pos, _, err := cryptoReadDER(der, 0) // 外层 SEQUENCE
	if err != nil {
		return nil, nil, err
	}
	if tag != 0x30 {
		return nil, nil, errors.New("not a DER SEQUENCE")
	}

	tag, pos, algLen, err := cryptoReadDER(der, pos) // AlgorithmIdentifier SEQUENCE
	if err != nil {
		return nil, nil, err
	}
	if tag != 0x30 {
		return nil, nil, errors.New("missing AlgorithmIdentifier")
	}
	pos += algLen // 跳过 OID + NULL 参数

	tag, body, _, err := cryptoReadDER(der, pos) // BIT STRING
	if err != nil {
		return nil, nil, err
	}
	if tag != 0x03 {
		return nil, nil, errors.New("missing BIT STRING")
	}
	if body >= len(der) {
		return nil, nil, errors.New("truncated DER")
	}
	if der[body] != 0x00 {
		return nil, nil, errors.New("unexpected unused bits in BIT STRING")
	}
	pos = body + 1

	tag, pos, _, err = cryptoReadDER(der, pos) // 内层 RSAPublicKey SEQUENCE
	if err != nil {
		return nil, nil, err
	}
	if tag != 0x30 {
		return nil, nil, errors.New("missing RSAPublicKey")
	}

	vals := make([]*big.Int, 0, 2)
	for i := 0; i < 2; i++ { // INTEGER n, INTEGER e
		var intTag byte
		var start, size int
		intTag, start, size, err = cryptoReadDER(der, pos)
		if err != nil {
			return nil, nil, err
		}
		if intTag != 0x02 {
			return nil, nil, errors.New("expected INTEGER")
		}
		if start > len(der) {
			return nil, nil, errors.New("truncated DER")
		}
		end := start + size
		if end > len(der) { // Python 对越界切片是静默截断
			end = len(der)
		}
		vals = append(vals, new(big.Int).SetBytes(der[start:end]))
		pos = start + size
	}
	return vals[0], vals[1], nil
}

// cryptoB64Decode 对应 Python base64.b64decode(x + "=" * (-len(x) % 4))：
// 先补齐 padding 再解码；失败时丢掉字母表之外的字符重试（Python 的
// validate=False 会静默忽略它们）。客户端的密文经常缺 padding。
func cryptoB64Decode(raw string) ([]byte, error) {
	var kept strings.Builder
	kept.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '+', r == '/', r == '=':
			kept.WriteRune(r)
		}
	}
	s := kept.String()
	padded := s + strings.Repeat("=", (4-len(s)%4)%4)
	if out, err := base64.StdEncoding.DecodeString(padded); err == nil {
		return out, nil
	}
	out, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	return out, nil
}

// RsaEncryptPKCS1v15 对应 Python rsa_encrypt：RSA/PKCS#1 v1.5 加密明文，
// 返回 base64 密文。rng 为 nil 时用 crypto/rand；非 nil 时每次填充尝试
// 从中读 k-len(msg)-3 字节，读到含 0x00 就重读（PKCS#1 要求填充非零）。
func RsaEncryptPKCS1v15(pubkeyDERB64, plaintext string, rng io.Reader) (string, error) {
	der, err := cryptoB64Decode(pubkeyDERB64)
	if err != nil {
		return "", err
	}
	n, e, err := DerToNumbers(der)
	if err != nil {
		return "", err
	}
	k := (n.BitLen() + 7) / 8
	msg := []byte(plaintext)
	if len(msg) > k-11 {
		return "", fmt.Errorf("message too long for RSA-%d", k*8)
	}

	psLen := k - len(msg) - 3
	if rng == nil {
		rng = rand.Reader
	}
	ps := make([]byte, psLen)
	for {
		if _, err := io.ReadFull(rng, ps); err != nil {
			return "", fmt.Errorf("rng produced wrong length: %w", err)
		}
		if bytes.IndexByte(ps, 0) < 0 {
			break
		}
	}

	block := make([]byte, 0, k)
	block = append(block, 0x00, 0x02)
	block = append(block, ps...)
	block = append(block, 0x00)
	block = append(block, msg...)

	c := new(big.Int).Exp(new(big.Int).SetBytes(block), e, n)
	raw := c.Bytes() // 等价于 c.to_bytes(k, "big")：左侧补零到 k 字节
	if len(raw) > k {
		return "", errors.New("ciphertext does not fit modulus")
	}
	ciphertext := make([]byte, k)
	copy(ciphertext[k-len(raw):], raw)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptClientEnv 对应 Python decrypt_client_env：解密 LOOMYENC1: 开头的
// blob（salt ‖ iv ‖ tag ‖ data，key = scrypt(口令, salt, 16384, 8, 1, 32)）；
// 不是密文的输入原样返回。
func DecryptClientEnv(raw string) (string, error) {
	if !strings.HasPrefix(raw, EnvCryptMagic+":") {
		return raw, nil
	}
	b64 := strings.TrimSpace(raw[len(EnvCryptMagic)+1:])
	payload, err := cryptoB64Decode(b64)
	if err != nil {
		return "", err
	}
	if len(payload) < SaltLen+IVLen+TagLen {
		return "", fmt.Errorf("client env payload too short: %d bytes", len(payload))
	}
	salt := payload[:SaltLen]
	iv := payload[SaltLen : SaltLen+IVLen]
	tag := payload[SaltLen+IVLen : SaltLen+IVLen+TagLen]
	data := payload[SaltLen+IVLen+TagLen:]

	key, err := Scrypt([]byte(EnvCryptPassphrase), salt, 16384, 8, 1, KeyLen)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ciphertext := make([]byte, 0, len(data)+len(tag))
	ciphertext = append(ciphertext, data...)
	ciphertext = append(ciphertext, tag...)
	plain, err := gcm.Open(nil, iv, ciphertext, nil)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(plain) {
		return "", errors.New("client env is not valid UTF-8")
	}
	return string(plain), nil
}

// cryptoEnvSplitLines 对应 Python str.splitlines()：\r\n / \r / \n 以及
// \v \f \x1c-\x1e NEL LS PS 都算行边界，末尾换行不产生尾随空行。
func cryptoEnvSplitLines(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		width := 0
		switch c {
		case '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e:
			width = 1
		case 0xc2: // U+0085 NEL 的 UTF-8 前导字节
			if i+1 < len(text) && text[i+1] == 0x85 {
				width = 2
			}
		case 0xe2: // U+2028 / U+2029 的 UTF-8 前导字节
			if i+2 < len(text) && text[i+1] == 0x80 && (text[i+2] == 0xa8 || text[i+2] == 0xa9) {
				width = 3
			}
		}
		if width == 0 {
			continue
		}
		if c == '\r' && i+1 < len(text) && text[i+1] == '\n' {
			width = 2
		}
		out = append(out, text[start:i])
		i += width - 1
		start = i + 1
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// ParseEnv 对应 Python parse_env：KEY=VALUE 逐行转 map（注释与空行忽略）。
func ParseEnv(text string) map[string]string {
	out := make(map[string]string)
	for _, line := range cryptoEnvSplitLines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}
