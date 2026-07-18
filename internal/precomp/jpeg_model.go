package precomp

// JPEG 量子化 DCT 係数の文脈モデル+レンジ符号化。
//
// zstd(LZ+FSE)は係数平面のゼロの塊は縮められるが、「隣接ブロックの同じ
// 周波数成分は相関する」という2次元の空間相関を使えない。ここでは各係数を
// 「左・上のブロックの同位置係数の大きさ」と「周波数帯」を文脈にしてレンジ
// 符号化する(lepton / H.264 CABAC と同じ発想)。これにより Huffman/zstd を
// 明確に下回れる。純Go・cgo不要。可逆(復号は係数を完全再現する)。

import "math/bits"

// 文脈の次元
const (
	nBandBucket = 12
	nActBucket  = 6
	maxUnary    = 13 // 8ビット JPEG の係数カテゴリ上限(DC差分で最大11)+余裕
)

// bandBucket は zigzag 位置 k を文脈帯に写す。低周波(k=1..8)は統計が
// はっきり異なるため各位置を独立の文脈にし、高周波はまとめる。
func bandBucket(k int) int {
	switch {
	case k <= 8:
		return k // 0..8(DC は 0)
	case k <= 12:
		return 9
	case k <= 27:
		return 10
	default:
		return 11
	}
}

func actBucket(a int) int {
	switch {
	case a == 0:
		return 0
	case a == 1:
		return 1
	case a <= 3:
		return 2
	case a <= 7:
		return 3
	case a <= 15:
		return 4
	default:
		return 5
	}
}

// jpegModels は全文脈のビットモデル集合。
type jpegModels struct {
	// mag[comp][band][act][unaryPos] = カテゴリ(大きさのビット長)を単進符号化
	mag []bitModel
	// sign[comp][band]
	sign []bitModel
	// mant[comp][band][cat][bitPos] = マンティッサビットを文脈符号化。
	// 実写真では上位マンティッサビットに偏りがあり 0.5 固定より縮む。
	mant []bitModel
	nc   int
}

func newJPEGModels(nc int) *jpegModels {
	m := &jpegModels{nc: nc}
	m.mag = newModels(nc * nBandBucket * nActBucket * maxUnary)
	m.sign = newModels(nc * nBandBucket)
	m.mant = newModels(nc * nBandBucket * maxUnary * maxUnary)
	return m
}

func (m *jpegModels) magIdx(ci, band, act, pos int) int {
	return ((ci*nBandBucket+band)*nActBucket+act)*maxUnary + pos
}
func (m *jpegModels) signIdx(ci, band int) int { return ci*nBandBucket + band }

// mantIdx: 文脈 = 成分 × 帯 × カテゴリ s × マンティッサビット位置 b。
func (m *jpegModels) mantIdx(ci, band, s, b int) int {
	return ((ci*nBandBucket+band)*maxUnary+min(s, maxUnary-1))*maxUnary + min(b, maxUnary-1)
}

// blockGrid は成分ごとに「ラスタ位置(bx,by)→ 復号順ブロック番号」を作る。
type blockGrid struct {
	w, h  int
	toDec []int // by*w+bx -> decodeIndex
}

func (f *jpegFrame) buildGrids() []blockGrid {
	grids := make([]blockGrid, len(f.comps))
	for ci := range f.comps {
		c := &f.comps[ci]
		w := f.mcuX * c.h
		h := f.mcuY * c.v
		g := blockGrid{w: w, h: h, toDec: make([]int, w*h)}
		for mcuy := 0; mcuy < f.mcuY; mcuy++ {
			for mcux := 0; mcux < f.mcuX; mcux++ {
				mcu := mcuy*f.mcuX + mcux
				for bv := 0; bv < c.v; bv++ {
					for bh := 0; bh < c.h; bh++ {
						b := bv*c.h + bh
						dec := mcu*c.blocksMC + b
						bx := mcux*c.h + bh
						by := mcuy*c.v + bv
						g.toDec[by*w+bx] = dec
					}
				}
			}
		}
		grids[ci] = g
	}
	return grids
}

// neighborAct は (ci,bx,by,k) の左・上・左上ブロックの同位置係数の |値| 合計。
// 左上を含めることで対角エッジの活性を捉え、文脈の弁別が上がる。
func neighborAct(coeff [][]int16, g *blockGrid, ci, bx, by, k int) int {
	a := 0
	if bx > 0 {
		a += absI16(coeff[ci][g.toDec[by*g.w+(bx-1)]*64+k])
	}
	if by > 0 {
		a += absI16(coeff[ci][g.toDec[(by-1)*g.w+bx]*64+k])
	}
	if bx > 0 && by > 0 {
		a += absI16(coeff[ci][g.toDec[(by-1)*g.w+(bx-1)]*64+k])
	}
	return a
}

func absI16(v int16) int {
	if v < 0 {
		return int(-v)
	}
	return int(v)
}

func iabs32(v int32) int {
	if v < 0 {
		return int(-v)
	}
	return int(v)
}

// dcResetStride は DC 予測器がリセットされる復号ブロック間隔(0=リセット無し)。
func (f *jpegFrame) dcResetStride(ci int) int {
	if f.restart <= 0 {
		return 0
	}
	return f.restart * f.comps[ci].blocksMC
}

// dcAbsFromDiff は走査順 DC 差分(coeff の k=0)から絶対 DC 値を復元する。
func dcAbsFromDiff(coeff []int16, stride int) []int32 {
	n := len(coeff) / 64
	abs := make([]int32, n)
	var acc int32
	for dec := 0; dec < n; dec++ {
		d := int32(coeff[dec*64])
		if dec == 0 || (stride > 0 && dec%stride == 0) {
			acc = d
		} else {
			acc += d
		}
		abs[dec] = acc
	}
	return abs
}

// dcDiffIntoCoeff は絶対 DC 値を走査順差分に戻して coeff の k=0 に書く。
func dcDiffIntoCoeff(coeff []int16, abs []int32, stride int) {
	for dec := 0; dec < len(abs); dec++ {
		var d int32
		if dec == 0 || (stride > 0 && dec%stride == 0) {
			d = abs[dec]
		} else {
			d = abs[dec] - abs[dec-1]
		}
		coeff[dec*64] = int16(d)
	}
}

// dcPredict は左・上・左上の絶対 DC から JPEG-LS 中央値予測子で予測し、
// 局所勾配を活性(文脈)として返す。中央値予測子は予測値が [min,max] に
// 収まることが保証され、残差が発散しない。
func dcPredict(abs []int32, g *blockGrid, bx, by int) (pred int32, act int) {
	hasL, hasU := bx > 0, by > 0
	hasUL := hasL && hasU
	var L, U, UL int32
	if hasL {
		L = abs[g.toDec[by*g.w+(bx-1)]]
	}
	if hasU {
		U = abs[g.toDec[(by-1)*g.w+bx]]
	}
	if hasUL {
		UL = abs[g.toDec[(by-1)*g.w+(bx-1)]]
	}
	switch {
	case hasL && hasU:
		mx, mn := L, U
		if mx < mn {
			mx, mn = mn, mx
		}
		switch {
		case UL >= mx:
			pred = mn
		case UL <= mn:
			pred = mx
		default:
			pred = L + U - UL
		}
		act = iabs32(L-UL) + iabs32(U-UL)
	case hasL:
		pred = L
	case hasU:
		pred = U
	}
	return pred, act
}

// encodeCoefficients は係数を文脈レンジ符号化する。
func (f *jpegFrame) encodeCoefficients(coeff [][]int16) []byte {
	grids := f.buildGrids()
	m := newJPEGModels(len(f.comps))
	e := newRangeEncoder()
	for ci := range f.comps {
		g := &grids[ci]
		// DC は絶対値に復元し、空間予測残差を符号化する。
		dcabs := dcAbsFromDiff(coeff[ci], f.dcResetStride(ci))
		for by := 0; by < g.h; by++ {
			for bx := 0; bx < g.w; bx++ {
				dec := g.toDec[by*g.w+bx]
				base := dec * 64
				// DC(k=0): 空間予測残差
				pred, dact := dcPredict(dcabs, g, bx, by)
				m.codeValue(e, ci, 0, actBucket(dact), int(dcabs[dec]-pred))
				// AC(k=1..63)
				for k := 1; k < 64; k++ {
					band := bandBucket(k)
					act := actBucket(neighborAct(coeff, g, ci, bx, by, k))
					m.codeValue(e, ci, band, act, int(coeff[ci][base+k]))
				}
			}
		}
	}
	return e.finish()
}

// codeValue は整数 v を文脈(成分・帯・活性)で符号化する。
func (m *jpegModels) codeValue(e *rangeEncoder, ci, band, act, v int) {
	av := v
	if av < 0 {
		av = -av
	}
	s := bits.Len(uint(av))
	for pos := 0; pos < s; pos++ {
		e.encodeBit(&m.mag[m.magIdx(ci, band, act, min(pos, maxUnary-1))], 1)
	}
	e.encodeBit(&m.mag[m.magIdx(ci, band, act, min(s, maxUnary-1))], 0)
	if s > 0 {
		mant := av - (1 << uint(s-1))
		for b := s - 2; b >= 0; b-- {
			e.encodeBit(&m.mant[m.mantIdx(ci, band, s, b)], (mant>>uint(b))&1)
		}
		sb := 0
		if v < 0 {
			sb = 1
		}
		e.encodeBit(&m.sign[m.signIdx(ci, band)], sb)
	}
}

// readValue は codeValue の逆。
func (m *jpegModels) readValue(d *rangeDecoder, ci, band, act int) int {
	s := 0
	for s < maxUnary {
		if d.decodeBit(&m.mag[m.magIdx(ci, band, act, min(s, maxUnary-1))]) == 0 {
			break
		}
		s++
	}
	if s == 0 {
		return 0
	}
	mant := 0
	for b := s - 2; b >= 0; b-- {
		mant |= d.decodeBit(&m.mant[m.mantIdx(ci, band, s, b)]) << uint(b)
	}
	av := (1 << uint(s-1)) + mant
	if d.decodeBit(&m.sign[m.signIdx(ci, band)]) == 1 {
		av = -av
	}
	return av
}

// decodeCoefficients は encodeCoefficients の逆(係数を完全再現)。
func (f *jpegFrame) decodeCoefficients(data []byte) [][]int16 {
	grids := f.buildGrids()
	m := newJPEGModels(len(f.comps))
	d := newRangeDecoder(data)
	nMCU := f.mcuX * f.mcuY
	coeff := make([][]int16, len(f.comps))
	for ci := range f.comps {
		coeff[ci] = make([]int16, nMCU*f.comps[ci].blocksMC*64)
	}
	for ci := range f.comps {
		g := &grids[ci]
		nBlk := len(coeff[ci]) / 64
		dcabs := make([]int32, nBlk)
		for by := 0; by < g.h; by++ {
			for bx := 0; bx < g.w; bx++ {
				dec := g.toDec[by*g.w+bx]
				base := dec * 64
				// DC(k=0): 空間予測残差から絶対値を復元
				pred, dact := dcPredict(dcabs, g, bx, by)
				dcabs[dec] = pred + int32(m.readValue(d, ci, 0, actBucket(dact)))
				// AC(k=1..63)
				for k := 1; k < 64; k++ {
					band := bandBucket(k)
					act := actBucket(neighborAct(coeff, g, ci, bx, by, k))
					coeff[ci][base+k] = int16(m.readValue(d, ci, band, act))
				}
			}
		}
		// 絶対 DC を走査順差分に戻す
		dcDiffIntoCoeff(coeff[ci], dcabs, f.dcResetStride(ci))
	}
	return coeff
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
