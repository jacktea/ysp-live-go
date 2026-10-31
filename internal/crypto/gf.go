package crypto

import "math/big"

// gfMul 复刻原项目 _gf_mult：GF(2^128) 乘法，模 x^128 + x^7 + x^2 + x + 1。
// 仅用于单元测试对照原 Python 实现，运行时不需要（标准库已内置 GCM）。
func gfMul(x, y *big.Int) *big.Int {
	z := new(big.Int)
	v := new(big.Int).Set(x)
	// 约简向量 R = 0xE1 << 120
	rVec := new(big.Int).Lsh(big.NewInt(0xE1), 120)
	for i := 0; i < 128; i++ {
		if y.Bit(i) == 1 {
			z.Xor(z, v)
		}
		lsb := v.Bit(0)
		v.Rsh(v, 1)
		if lsb == 1 {
			v.Xor(v, rVec)
		}
	}
	return z
}
