package loomy

import (
	"encoding/hex"
	"testing"
)

// 金向量来自本机 Python 3.13 的 hashlib.scrypt（RFC 7914 §12 的前两条，以及
// 客户端 .env.prod 实际使用的参数），原样保留。

func TestScryptRFC7914VectorEmptyPassword(t *testing.T) {
	got, err := Scrypt([]byte{}, []byte{}, 16, 1, 1, 64)
	if err != nil {
		t.Fatalf("Scrypt: %v", err)
	}
	want := "77d6576238657b203b19ca42c18a0497f16b4844e3074ae8dfdffa3fede21442" +
		"fcd0069ded0948f8326a753a0fc81f17e8d3e0fb2e0d3628cf35e20c38d18906"
	if have := hex.EncodeToString(got); have != want {
		t.Fatalf("scrypt([]) = %s, want %s", have, want)
	}
}

func TestScryptRFC7914VectorPasswordNaCl(t *testing.T) {
	got, err := Scrypt([]byte("password"), []byte("NaCl"), 1024, 8, 16, 64)
	if err != nil {
		t.Fatalf("Scrypt: %v", err)
	}
	want := "fdbabe1c9d3472007856e7190d01e9fe7c6ad7cbc8237830e77376634b373162" +
		"2eaf30d92e22a3886ff109279d9830dac727afb94a83ee6d8360cbdfa2cc0640"
	if have := hex.EncodeToString(got); have != want {
		t.Fatalf("scrypt(password, NaCl) = %s, want %s", have, want)
	}
}

func TestScryptClientEnvPassphrase(t *testing.T) {
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
	got, err := Scrypt([]byte(EnvCryptPassphrase), salt, 16384, 8, 1, 32)
	if err != nil {
		t.Fatalf("Scrypt: %v", err)
	}
	want := "0955058420c85e9db0a5fd29802a9428598b1b39e293a2cba8d17b2aaccef0e8"
	if have := hex.EncodeToString(got); have != want {
		t.Fatalf("scrypt(env passphrase) = %s, want %s", have, want)
	}
}

func TestScryptRejectsInvalidParams(t *testing.T) {
	cases := []struct {
		name           string
		N, r, p, dkLen int
	}{
		{"N not a power of two", 15, 8, 1, 32},
		{"N too small", 1, 8, 1, 32},
		{"r zero", 1024, 0, 1, 32},
		{"p zero", 1024, 8, 0, 32},
		{"negative dkLen", 1024, 8, 1, -1},
	}
	for _, c := range cases {
		if _, err := Scrypt([]byte("pw"), []byte("salt"), c.N, c.r, c.p, c.dkLen); err == nil {
			t.Fatalf("%s: expected an error", c.name)
		}
	}
}
