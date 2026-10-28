package loomy

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 对应 Python loomy2api/signer.py —— 讯飞账号服务的请求签名（HMAC-SHA1）。
// 逐行移植 electron/xfyun/sign.js：
//
//	stringToSign = "\n".join([METHOD, PATH, QUERY, Content-MD5, Content-Type,
//	                          Date, Nonce, SignedHeaders, CanonicalizedHeaders])
//	signature    = base64(hmac_sha1(accessKeySecret, stringToSign))
//	Authorization = "account <accessKeyId>:<signature>"
//
// 没有自定义 x-* 头时最后两行都是空串，但九行结构不变（字符串以 "\n\n" 收尾）。

// signerContentType 对应 Python 的 content_type 默认值：传空串视为未指定。
func signerContentType(contentType string) string {
	if contentType == "" {
		return "application/json"
	}
	return contentType
}

// signerEscape 对应 Python signer._escape：等价于 quote(s, safe="-_.~")。
// urllib.parse.quote 永远保留 A-Za-z0-9 与 - _ . ~，其余按 UTF-8 字节转 %XX
// （十六进制大写），因此空格是 %20 而不是 +，! ' ( ) * 落到 %21 %27 %28 %29 %2A。
func signerEscape(value string) string {
	const upperHex = "0123456789ABCDEF"
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			out.WriteByte(c)
		default:
			out.WriteByte('%')
			out.WriteByte(upperHex[c>>4])
			out.WriteByte(upperHex[c&0x0f])
		}
	}
	return out.String()
}

// ContentMD5 对应 Python content_md5：base64(md5(body))，空 body 返回空串。
func ContentMD5(body string) string {
	if body == "" {
		return ""
	}
	sum := md5.Sum([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// signerEscapedPath 对应 Python _escaped_path：补前导 "/"、去掉尾部 "/"（长度 >1 时），
// 然后按 "/" 分段转义再拼回。
func signerEscapedPath(path string) string {
	p := path
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if seg != "" {
			segs[i] = signerEscape(seg)
		}
	}
	return strings.Join(segs, "/")
}

// signerEscapedQuery 对应 Python _escaped_query。Python dict 保留插入序，
// Go map 无序，故按 key 排序后拼接（对单次调用内的行为是确定的）。
func signerEscapedQuery(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, signerEscape(k)+"="+signerEscape(params[k]))
	}
	return strings.Join(parts, "&")
}

// signerCanonicalized 对应 Python _canonicalized：挑出 x- 前缀头，
// 键转小写、值去首尾空白，返回 (SignedHeaders, CanonicalizedHeaders)。
// 没有匹配项时两者都是空串。
func signerCanonicalized(headers map[string]string, prefix string) (string, string) {
	if len(headers) == 0 {
		return "", ""
	}
	// Python 的 dict 推导按插入序覆盖大小写重复的键；Go map 无序，
	// 这里按原始键排序遍历，让「后者覆盖前者」的结果保持确定。
	rawKeys := make([]string, 0, len(headers))
	for k := range headers {
		rawKeys = append(rawKeys, k)
	}
	sort.Strings(rawKeys)

	picked := make(map[string]string, len(headers))
	for _, k := range rawKeys {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, prefix) {
			picked[lower] = strings.TrimSpace(headers[k])
		}
	}
	if len(picked) == 0 {
		return "", ""
	}

	keys := make([]string, 0, len(picked))
	for k := range picked {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonical strings.Builder
	for _, k := range keys {
		canonical.WriteString(k)
		canonical.WriteByte(':')
		canonical.WriteString(picked[k])
		canonical.WriteByte('\n')
	}
	return strings.Join(keys, ";"), canonical.String()
}

// StringToSign 对应 Python string_to_sign：九行、\n 连接，最后一行去掉尾部 \n。
func StringToSign(method, path string, query map[string]string, body, contentType, date, nonce string, customHeaders map[string]string) string {
	signedHeaders, canonical := signerCanonicalized(customHeaders, "x-")
	return strings.Join([]string{
		strings.ToUpper(method),
		signerEscapedPath(path),
		signerEscapedQuery(query),
		ContentMD5(body),
		signerContentType(contentType),
		date,
		nonce,
		signedHeaders,
		strings.TrimRight(canonical, "\n"),
	}, "\n")
}

// Sign 对应 Python sign：base64(hmac_sha1(secret, string_to_sign))。
func Sign(secret, method, path string, query map[string]string, body, contentType, date, nonce string, customHeaders map[string]string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(StringToSign(method, path, query, body, contentType, date, nonce, customHeaders)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// signerUUID 对应 Python str(uuid.uuid4())：小写、带连字符的 v4 UUID。
func signerUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand 在 Go 1.24+ 不会失败（内部直接 panic）
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// BuildHeaders 对应 Python build_headers：一次账号服务请求所需的完整头集合。
// Date 用当前 UTC 时间，Nonce 用随机 UUID；body 非空时补 Content-MD5。
func BuildHeaders(accessKeyID, accessKeySecret, method, path, body string, query map[string]string, contentType string, customHeaders map[string]string) map[string]string {
	ct := signerContentType(contentType)
	date := time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	nonce := signerUUID()
	signature := Sign(accessKeySecret, method, path, query, body, ct, date, nonce, customHeaders)

	headers := map[string]string{
		"Authorization": "account " + accessKeyID + ":" + signature,
		"Date":          date,
		"Nonce":         nonce,
		"Content-Type":  ct,
	}
	if digest := ContentMD5(body); digest != "" {
		headers["Content-MD5"] = digest
	}
	for k, v := range customHeaders {
		headers[k] = v
	}
	return headers
}
