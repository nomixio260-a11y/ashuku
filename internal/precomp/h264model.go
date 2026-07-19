package precomp

// H.264 構文要素の文脈適応モデル。
//
// CAVLC の固定 VLC 表を、適応確率の二値算術符号(rangecoder.go)に置き換える。
// 各構文要素種別ごとに numCtx(適応ユーナリ+ガンマエスケープの数値コーダ)か
// bitModel を持ち、レベルは suffixLength、係数トークンは nC のバケットで
// 文脈を分ける — CABAC が CAVLC より 2〜9% 小さいのと同じ原理を、
// より新しい適応(LZMA 型 12bit 確率・シフト更新)で追う。

// numCtx は非負整数の適応符号(ユーナリ K + ガンマエスケープ)。
type numCtx struct {
	unary  []bitModel
	escLen []bitModel
	escBit []bitModel
}

const numCtxK = 8

func newNumCtx() *numCtx {
	return &numCtx{
		unary:  newModels(numCtxK),
		escLen: newModels(32),
		escBit: newModels(32),
	}
}

func (c *numCtx) encode(e *rangeEncoder, v uint32) {
	for i := 0; i < numCtxK; i++ {
		if v > uint32(i) {
			e.encodeBit(&c.unary[i], 1)
		} else {
			e.encodeBit(&c.unary[i], 0)
			return
		}
	}
	// v >= K: rem = v-K をガンマ符号(長さユーナリ+下位ビット)
	rem := v - numCtxK + 1 // ≥1
	n := 0
	for t := rem; t > 1; t >>= 1 {
		n++
	}
	for i := 0; i < n; i++ {
		e.encodeBit(&c.escLen[i], 1)
	}
	e.encodeBit(&c.escLen[n], 0)
	for i := n - 1; i >= 0; i-- {
		e.encodeBit(&c.escBit[i], int((rem>>uint(i))&1))
	}
}

func (c *numCtx) decode(d *rangeDecoder) uint32 {
	for i := 0; i < numCtxK; i++ {
		if d.decodeBit(&c.unary[i]) == 0 {
			return uint32(i)
		}
	}
	n := 0
	for n < 32 && d.decodeBit(&c.escLen[n]) == 1 {
		n++
	}
	rem := uint32(1)
	for i := n - 1; i >= 0; i-- {
		rem = rem<<1 | uint32(d.decodeBit(&c.escBit[i]))
	}
	return rem + numCtxK - 1
}

// zigzag は符号付き→非負(0,-1,1,-2,2… → 0,1,2,3,4…)。
func zigzagS(v int32) uint32 {
	if v <= 0 {
		return uint32(-v) * 2
	}
	return uint32(v)*2 - 1
}

func unzigzagS(u uint32) int32 {
	if u%2 == 0 {
		return -int32(u / 2)
	}
	return int32(u/2) + 1
}

// h264Model は1ストリーム分の全文脈(スライスをまたいで適応が持続する)。
type h264Model struct {
	skipRun   *numCtx
	mbTypeI   *numCtx
	mbTypeP   *numCtx
	subMbType *numCtx
	refIdx    *numCtx
	mvd       [2]*numCtx // x, y
	prevIntra []bitModel // [1]
	remIntra  []bitModel // [3]
	chromaPr  *numCtx
	cbpBitsI  []bitModel // [6]
	cbpBitsP  []bitModel // [6]
	qpDelta   *numCtx
	contBit   []bitModel // [2] 0=skip後, 1=MB後

	tc       [6]*numCtx    // nC バケット: -1,0,1,2-3,4-7,8+
	t1Bits   [6][]bitModel // 各2ビット
	t1Sign   []bitModel    // [3]
	level    [7]*numCtx    // suffixLength 0..6
	tzWide   [4]*numCtx    // totalZeros: tc 1,2,3-6,7+
	tzChroma *numCtx
	runB     [4]*numCtx // zerosLeft 1,2,3-6,7+
}

func newH264Model() *h264Model {
	m := &h264Model{
		skipRun:   newNumCtx(),
		mbTypeI:   newNumCtx(),
		mbTypeP:   newNumCtx(),
		subMbType: newNumCtx(),
		refIdx:    newNumCtx(),
		prevIntra: newModels(1),
		remIntra:  newModels(3),
		chromaPr:  newNumCtx(),
		cbpBitsI:  newModels(6),
		cbpBitsP:  newModels(6),
		qpDelta:   newNumCtx(),
		contBit:   newModels(2),
		t1Sign:    newModels(3),
		tzChroma:  newNumCtx(),
	}
	m.mvd[0], m.mvd[1] = newNumCtx(), newNumCtx()
	for i := range m.tc {
		m.tc[i] = newNumCtx()
		m.t1Bits[i] = newModels(2)
	}
	for i := range m.level {
		m.level[i] = newNumCtx()
	}
	for i := range m.tzWide {
		m.tzWide[i] = newNumCtx()
	}
	for i := range m.runB {
		m.runB[i] = newNumCtx()
	}
	return m
}

// nC → tc 文脈バケット。
func nCBucket(nC int) int {
	switch {
	case nC < 0:
		return 0
	case nC == 0:
		return 1
	case nC == 1:
		return 2
	case nC <= 3:
		return 3
	case nC <= 7:
		return 4
	}
	return 5
}

func tcBucket(tc int) int {
	switch {
	case tc == 1:
		return 0
	case tc == 2:
		return 1
	case tc <= 6:
		return 2
	}
	return 3
}

func zlBucket(zl int) int {
	switch {
	case zl == 1:
		return 0
	case zl == 2:
		return 1
	case zl <= 6:
		return 2
	}
	return 3
}
