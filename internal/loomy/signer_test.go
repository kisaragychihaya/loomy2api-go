package loomy

import (
	"regexp"
	"testing"
	"time"
)

// 移植自 tests/test_signer.py。期望值来自客户端 sign.js 本身跑出来的金向量，
// 原样保留；回归会立刻被抓到。

func TestStringToSignPlainBody(t *testing.T) {
	expected := "POST\n" +
		"/login/account/getPuKey\n" +
		"\n" +
		"ZXFsDbtKIeoeelrBB+sO7g==\n" +
		"application/json\n" +
		"Sat, 26 Sep 2026 12:00:00 GMT\n" +
		"11111111-2222-3333-4444-555555555555\n" +
		"\n"

	got := StringToSign(
		"post", "/login/account/getPuKey", nil,
		`{"base":{"appid":"GM3LOOMY"}}`, "", // "" → Python 默认 application/json
		"Sat, 26 Sep 2026 12:00:00 GMT",
		"11111111-2222-3333-4444-555555555555",
		nil,
	)
	if got != expected {
		t.Fatalf("string_to_sign mismatch\n got: %q\nwant: %q", got, expected)
	}
}

func TestSignSignatureMatchesNode(t *testing.T) {
	sig := Sign(
		"TEST_SECRET", "POST", "/login/account/getPuKey", nil,
		`{"base":{"appid":"GM3LOOMY"}}`, "",
		"Sat, 26 Sep 2026 12:00:00 GMT",
		"11111111-2222-3333-4444-555555555555",
		nil,
	)
	if want := "hu4CwF8SGwW9+0aCwfU7idd7gho="; sig != want {
		t.Fatalf("signature = %q, want %q", sig, want)
	}
}

func TestSignQueryEscapingAndCustomHeaders(t *testing.T) {
	expected := "POST\n" +
		"/login/phone/checkCode\n" +
		"a=1%202&b=x%28y%29\n" +
		"\n" +
		"application/json\n" +
		"Sat, 26 Sep 2026 12:00:00 GMT\n" +
		"nonce-2\n" +
		"x-other;x-test\n" +
		"x-other:2\n" +
		"x-test:v1"

	query := map[string]string{"a": "1 2", "b": "x(y)"}
	custom := map[string]string{"X-Test": " v1 ", "x-other": "2"}

	got := StringToSign("POST", "/login/phone/checkCode/", query, "",
		"", "Sat, 26 Sep 2026 12:00:00 GMT", "nonce-2", custom)
	if got != expected {
		t.Fatalf("string_to_sign mismatch\n got: %q\nwant: %q", got, expected)
	}

	sig := Sign("TEST_SECRET", "POST", "/login/phone/checkCode/", query, "",
		"", "Sat, 26 Sep 2026 12:00:00 GMT", "nonce-2", custom)
	if want := "53bKaJEgHxiy8d7lYUMWFhmLtY0="; sig != want {
		t.Fatalf("signature = %q, want %q", sig, want)
	}
}

func TestStringToSignTrailingSlashIsStripped(t *testing.T) {
	a := StringToSign("POST", "/a/b/", nil, "", "", "d", "n", nil)
	b := StringToSign("POST", "/a/b", nil, "", "", "d", "n", nil)
	if a != b {
		t.Fatalf("trailing slash not stripped:\n%q\n%q", a, b)
	}
}

func TestStringToSignEmptyBodyHasEmptyMD5(t *testing.T) {
	if got := ContentMD5(""); got != "" {
		t.Fatalf("ContentMD5(\"\") = %q, want empty", got)
	}
	if want, got := "kAFQmDzST7DWlj99KOF/cg==", ContentMD5("abc"); got != want {
		t.Fatalf("ContentMD5(\"abc\") = %q, want %q", got, want)
	}
}

// 额外覆盖 _escape 的保留/转义表（Python quote(safe="-_.~") 的等价物）。
func TestSignerEscapeTable(t *testing.T) {
	cases := map[string]string{
		`a b!c'd(e)f*g~h-i_j.k l/m?n#o%p+q&r=s`: "a%20b%21c%27d%28e%29f%2Ag~h-i_j.k%20l%2Fm%3Fn%23o%25p%2Bq%26r%3Ds",
		"中":                                     "%E4%B8%AD",
		"":                                      "",
	}
	for in, want := range cases {
		if got := signerEscape(in); got != want {
			t.Fatalf("signerEscape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildHeadersShape(t *testing.T) {
	headers := BuildHeaders("AKID", "SECRET", "POST", "/login/account/getPuKey",
		`{"x":1}`, nil, "", nil)

	if auth := headers["Authorization"]; len(auth) < len("account AKID:") || auth[:len("account AKID:")] != "account AKID:" {
		t.Fatalf("Authorization = %q, want prefix %q", auth, "account AKID:")
	}
	if headers["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type = %q", headers["Content-Type"])
	}
	if _, ok := headers["Content-MD5"]; !ok {
		t.Fatal("missing Content-MD5 header")
	}
	date, ok := headers["Date"]
	if !ok {
		t.Fatal("missing Date header")
	}
	if _, err := time.Parse("Mon, 02 Jan 2006 15:04:05 GMT", date); err != nil {
		t.Fatalf("Date %q does not parse: %v", date, err)
	}
	nonce, ok := headers["Nonce"]
	if !ok {
		t.Fatal("missing Nonce header")
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(nonce) {
		t.Fatalf("Nonce %q is not a v4 UUID", nonce)
	}
	// Content-MD5 必须与签名里用的值一致。
	if want := ContentMD5(`{"x":1}`); headers["Content-MD5"] != want {
		t.Fatalf("Content-MD5 = %q, want %q", headers["Content-MD5"], want)
	}
}

func TestBuildHeadersNoMD5WithoutBody(t *testing.T) {
	headers := BuildHeaders("AKID", "SECRET", "POST", "/x", "", nil, "", nil)
	if _, ok := headers["Content-MD5"]; ok {
		t.Fatalf("unexpected Content-MD5 header: %q", headers["Content-MD5"])
	}
}
