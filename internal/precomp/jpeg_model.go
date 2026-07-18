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
	nBandBucket = 7
	nActBucket  = 4
	maxUnary    = 13 // 8ビット JPEG の係数カテゴリ上限(DC差分で最大11)+余裕
)

func bandBucket(k int) int {
	switch {
	case k == 0:
		return 0
	case k <= 2:
		return 1
	case k <= 5:
		return 2
	case k <= 11:
		return 3
	case k <= 20:
		return 4
	case k <= 32:
		return 5
	default:
		return 6
	}
}

func actBucket(a int) int {
	switch {
	case a == 0:
		return 0
	case a <= 1:
		return 1
	case a <= 4:
		return 2
	default:
		return 3
	}
}

// jpegModels は全文脈のビットモデル集合。
type jpegModels struct {
	// mag[comp][band][act][unaryPos] = カテゴリ(大きさのビット長)を単進符号化
	mag []bitModel
	// sign[comp][band]
	sign []bitModel
	nc   int
}

func newJPEGModels(nc int) *jpegModels {
	m := &jpegModels{nc: nc}
	m.mag = newModels(nc * nBandBucket * nActBucket * maxUnary)
	m.sign = newModels(nc * nBandBucket)
	return m
}

func (m *jpegModels) magIdx(ci, band, act, pos int) int {
	return ((ci*nBandBucket+band)*nActBucket+act)*maxUnary + pos
}
func (m *jpegModels) signIdx(ci, band int) int { return ci*nBandBucket + band }

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

// neighborAct は (ci,bx,by,k) の左・上ブロックの同位置係数の |値| 合計。
func neighborAct(coeff [][]int16, g *blockGrid, ci, bx, by, k int) int {
	a := 0
	if bx > 0 {
		a += absI16(coeff[ci][g.toDec[by*g.w+(bx-1)]*64+k])
	}
	if by > 0 {
		a += absI16(coeff[ci][g.toDec[(by-1)*g.w+bx]*64+k])
	}
	return a
}

func absI16(v int16) int {
	if v < 0 {
		return int(-v)
	}
	return int(v)
}

// encodeCoefficients は係数を文脈レンジ符号化する。
func (f *jpegFrame) encodeCoefficients(coeff [][]int16) []byte {
	grids := f.buildGrids()
	m := newJPEGModels(len(f.comps))
	e := newRangeEncoder()
	for ci := range f.comps {
		g := &grids[ci]
		for by := 0; by < g.h; by++ {
			for bx := 0; bx < g.w; bx++ {
				base := g.toDec[by*g.w+bx] * 64
				for k := 0; k < 64; k++ {
					v := int(coeff[ci][base+k])
					band := bandBucket(k)
					act := actBucket(neighborAct(coeff, g, ci, bx, by, k))
					// 大きさカテゴリ s を単進符号化
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
						// マンティッサ(上位ビットを除いた s-1 ビット)
						mant := av - (1 << uint(s-1))
						for b := s - 2; b >= 0; b-- {
							e.encodeBitEq((mant >> uint(b)) & 1)
						}
						// 符号
						sb := 0
						if v < 0 {
							sb = 1
						}
						e.encodeBit(&m.sign[m.signIdx(ci, band)], sb)
					}
				}
			}
		}
	}
	return e.finish()
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
		for by := 0; by < g.h; by++ {
			for bx := 0; bx < g.w; bx++ {
				base := g.toDec[by*g.w+bx] * 64
				for k := 0; k < 64; k++ {
					band := bandBucket(k)
					act := actBucket(neighborAct(coeff, g, ci, bx, by, k))
					s := 0
					for s < maxUnary {
						if d.decodeBit(&m.mag[m.magIdx(ci, band, act, min(s, maxUnary-1))]) == 0 {
							break
						}
						s++
					}
					if s == 0 {
						continue // 係数はゼロ
					}
					mant := 0
					for b := s - 2; b >= 0; b-- {
						mant |= d.decodeBitEq() << uint(b)
					}
					av := (1 << uint(s-1)) + mant
					if d.decodeBit(&m.sign[m.signIdx(ci, band)]) == 1 {
						av = -av
					}
					coeff[ci][base+k] = int16(av)
				}
			}
		}
	}
	return coeff
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
