package loomy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
)

// RFC 7914 的 scrypt 纯 Go 实现（只用 crypto/sha256、encoding/binary、math/bits）。
//
//	B    = PBKDF2-HMAC-SHA256(password, salt, 1, p * 128 * r)
//	B[i] = ROMix(B[i], N);  i = 0..p-1
//	Key  = PBKDF2-HMAC-SHA256(password, B, 1, dkLen)
//
// 用于解密客户端 .env.prod（N=16384, r=8, p=1, dkLen=32，约 16 MiB 内存）。

// Scrypt 按 RFC 7914 派生密钥：N 必须是 >1 的 2 的幂，r、p 必须为正。
func Scrypt(password, salt []byte, N, r, p, dkLen int) ([]byte, error) {
	if N <= 1 || N&(N-1) != 0 {
		return nil, errors.New("scrypt: N must be > 1 and a power of 2")
	}
	if r <= 0 || p <= 0 {
		return nil, errors.New("scrypt: r and p must be positive")
	}
	if dkLen < 0 {
		return nil, errors.New("scrypt: dkLen must not be negative")
	}
	// 与 x/crypto/scrypt 相同的溢出保护：r*p < 2^30 且各乘法不越界 int。
	if uint64(r)*uint64(p) >= 1<<30 || r > math.MaxInt/128/p || r > math.MaxInt/256 || N > math.MaxInt/128/r {
		return nil, errors.New("scrypt: parameters are too large")
	}

	blen := 128 * r
	b := scryptPBKDF2SHA256(password, salt, 1, p*blen)
	for i := 0; i < p; i++ {
		scryptROMix(b[i*blen:(i+1)*blen], N, r)
	}
	return scryptPBKDF2SHA256(password, b, 1, dkLen), nil
}

// scryptPBKDF2SHA256 是 PBKDF2-HMAC-SHA256（RFC 8018）；scrypt 只用 iterations=1。
func scryptPBKDF2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	numBlocks := (keyLen + hLen - 1) / hLen
	dk := make([]byte, 0, numBlocks*hLen)

	u := make([]byte, 0, hLen)
	t := make([]byte, hLen)
	var counter [4]byte
	for block := 1; block <= numBlocks; block++ {
		binary.BigEndian.PutUint32(counter[:], uint32(block))
		prf.Reset()
		prf.Write(salt)
		prf.Write(counter[:])
		u = prf.Sum(u[:0])
		copy(t, u)
		for n := 2; n <= iterations; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

// scryptROMix 对应 RFC 7914 的 ROMix：先用 BlockMix 填满 V，再做 N 次随机索引混合。
// b 是 128*r 字节的块，原地改写。
func scryptROMix(b []byte, N, r int) {
	blen := 128 * r
	v := make([]byte, blen*N)
	x := make([]byte, blen) // 当前块 X
	y := make([]byte, blen) // BlockMix 输出
	copy(x, b)

	for i := 0; i < N; i++ {
		copy(v[i*blen:(i+1)*blen], x)
		scryptBlockMix(y, x, r)
		copy(x, y)
	}
	for i := 0; i < N; i++ {
		j := int(binary.LittleEndian.Uint64(x[(2*r-1)*64:]) % uint64(N))
		vj := v[j*blen : (j+1)*blen]
		for k := 0; k < blen; k++ {
			y[k] = x[k] ^ vj[k]
		}
		scryptBlockMix(x, y, r)
	}
	copy(b, x)
}

// scryptBlockMix 对应 RFC 7914 的 BlockMix：把 2r 个 64 字节块按
// (Y[0], Y[2], ..., Y[2r-2], Y[1], Y[3], ..., Y[2r-1]) 重排写入 dst。
// src 与 dst 不能重叠。
func scryptBlockMix(dst, src []byte, r int) {
	var z, t [16]uint32
	last := src[(2*r-1)*64:]
	for i := range z {
		z[i] = binary.LittleEndian.Uint32(last[i*4:])
	}
	for i := 0; i < 2*r; i++ {
		blk := src[i*64 : i*64+64]
		for j := range t {
			t[j] = z[j] ^ binary.LittleEndian.Uint32(blk[j*4:])
		}
		scryptSalsa208(&t)
		z = t
		out := i / 2
		if i%2 != 0 {
			out += r
		}
		block := dst[out*64 : out*64+64]
		for j := range z {
			binary.LittleEndian.PutUint32(block[j*4:], z[j])
		}
	}
}

// scryptSalsa208 是 Salsa20/8 核心：8 轮（4 次双轮）后与原值相加，原地改写。
func scryptSalsa208(a *[16]uint32) {
	x0, x1, x2, x3 := a[0], a[1], a[2], a[3]
	x4, x5, x6, x7 := a[4], a[5], a[6], a[7]
	x8, x9, x10, x11 := a[8], a[9], a[10], a[11]
	x12, x13, x14, x15 := a[12], a[13], a[14], a[15]

	for i := 0; i < 8; i += 2 {
		// 列轮
		x4 ^= bits.RotateLeft32(x0+x12, 7)
		x8 ^= bits.RotateLeft32(x4+x0, 9)
		x12 ^= bits.RotateLeft32(x8+x4, 13)
		x0 ^= bits.RotateLeft32(x12+x8, 18)

		x9 ^= bits.RotateLeft32(x5+x1, 7)
		x13 ^= bits.RotateLeft32(x9+x5, 9)
		x1 ^= bits.RotateLeft32(x13+x9, 13)
		x5 ^= bits.RotateLeft32(x1+x13, 18)

		x14 ^= bits.RotateLeft32(x10+x6, 7)
		x2 ^= bits.RotateLeft32(x14+x10, 9)
		x6 ^= bits.RotateLeft32(x2+x14, 13)
		x10 ^= bits.RotateLeft32(x6+x2, 18)

		x3 ^= bits.RotateLeft32(x15+x11, 7)
		x7 ^= bits.RotateLeft32(x3+x15, 9)
		x11 ^= bits.RotateLeft32(x7+x3, 13)
		x15 ^= bits.RotateLeft32(x11+x7, 18)

		// 行轮
		x1 ^= bits.RotateLeft32(x0+x3, 7)
		x2 ^= bits.RotateLeft32(x1+x0, 9)
		x3 ^= bits.RotateLeft32(x2+x1, 13)
		x0 ^= bits.RotateLeft32(x3+x2, 18)

		x6 ^= bits.RotateLeft32(x5+x4, 7)
		x7 ^= bits.RotateLeft32(x6+x5, 9)
		x4 ^= bits.RotateLeft32(x7+x6, 13)
		x5 ^= bits.RotateLeft32(x4+x7, 18)

		x11 ^= bits.RotateLeft32(x10+x9, 7)
		x8 ^= bits.RotateLeft32(x11+x10, 9)
		x9 ^= bits.RotateLeft32(x8+x11, 13)
		x10 ^= bits.RotateLeft32(x9+x8, 18)

		x12 ^= bits.RotateLeft32(x15+x14, 7)
		x13 ^= bits.RotateLeft32(x12+x15, 9)
		x14 ^= bits.RotateLeft32(x13+x12, 13)
		x15 ^= bits.RotateLeft32(x14+x13, 18)
	}

	a[0] += x0
	a[1] += x1
	a[2] += x2
	a[3] += x3
	a[4] += x4
	a[5] += x5
	a[6] += x6
	a[7] += x7
	a[8] += x8
	a[9] += x9
	a[10] += x10
	a[11] += x11
	a[12] += x12
	a[13] += x13
	a[14] += x14
	a[15] += x15
}
