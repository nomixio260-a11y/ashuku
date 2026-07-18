package precomp

// BMP(非圧縮ビットマップ)の可逆分解。
//
// 非圧縮 BMP(BI_RGB)は生ピクセルがそのまま並ぶだけで空間相関を全く
// 使っていない。PNG が小さいのはまさにこの相関を「行フィルタ」で除くから。
// そこで BMP のピクセル配列に PNG と同じ**行ごとの適応予測フィルタ**
// (None/Sub/Up/Average/Paeth を行ごとに最小残差で選択)をかけて残差にし、
// store の zstd/brotli に圧縮させる。実測で生ピクセルの ~7 割まで縮む。
//
// 可逆性: フィルタ↔逆フィルタは 256 の剰余環での全単射(丸めなし)で、
// 両方向を自前で持つ最も安全な変換。縮む見込みの時だけ採用+ SHA-256 検証。
// 純Go・cgo 不要。

import (
	"encoding/binary"
	"errors"
)

// IsBMP は BM シグネチャを判定する。
func IsBMP(head []byte) bool {
	return len(head) >= 2 && head[0] == 'B' && head[1] == 'M'
}

// BMPRecipe は BMP 再構成レシピ。
type BMPRecipe struct {
	PrefixLen int     `json:"pfx"` // ピクセル配列直前までのスケルトン長
	Suffix    []byte  `json:"sfx"` // ピクセル配列以降の原文
	RowBytes  int     `json:"rb"`  // 1行のバイト数(4バイト境界パディング込み)
	Rows      int     `json:"rows"`
	Step      int     `json:"step"`    // フィルタのバイト間隔(= bpp/8)
	Filters   []uint8 `json:"filters"` // 行ごとのフィルタ種別(0..4)
}

// BMPUnwrapped は分解結果。
type BMPUnwrapped struct {
	Chunked []byte
	Recipe  *BMPRecipe
}

// TryUnwrapBMP は非圧縮 BMP のピクセル配列を行予測残差に変換する。
func TryUnwrapBMP(orig []byte, maxPlain int64) (*BMPUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if len(orig) < 54 || !IsBMP(orig) {
		return nil, false
	}
	dataOff := int(binary.LittleEndian.Uint32(orig[10:]))
	dibSize := int(binary.LittleEndian.Uint32(orig[14:]))
	if dibSize < 40 || 14+dibSize > len(orig) {
		return nil, false
	}
	width := int(int32(binary.LittleEndian.Uint32(orig[18:])))
	height := int(int32(binary.LittleEndian.Uint32(orig[22:])))
	bpp := int(binary.LittleEndian.Uint16(orig[28:]))
	comp := int(binary.LittleEndian.Uint32(orig[30:]))
	if comp != 0 { // BI_RGB(非圧縮)のみ
		return nil, false
	}
	if bpp != 8 && bpp != 24 && bpp != 32 {
		return nil, false
	}
	if width <= 0 || height == 0 {
		return nil, false
	}
	rows := height
	if rows < 0 {
		rows = -rows // トップダウン
	}
	rowBytes := ((width*bpp + 31) / 32) * 4
	step := bpp / 8
	pixelBytes := rowBytes * rows
	if dataOff < 54 || dataOff+pixelBytes > len(orig) || rows < 4 || int64(pixelBytes) > maxPlain {
		return nil, false
	}

	pix := orig[dataOff : dataOff+pixelBytes]
	filtered := make([]byte, pixelBytes)
	filters := make([]uint8, rows)
	prevRow := make([]byte, rowBytes) // 上の行(初期はゼロ)
	tmp := make([]byte, rowBytes)
	for r := 0; r < rows; r++ {
		row := pix[r*rowBytes : (r+1)*rowBytes]
		ft := bestRowFilter(row, prevRow, step, tmp)
		filters[r] = ft
		applyRowFilter(ft, row, prevRow, step, filtered[r*rowBytes:(r+1)*rowBytes])
		prevRow = row
	}

	prefix := orig[:dataOff]
	suffix := append([]byte(nil), orig[dataOff+pixelBytes:]...)
	chunked := make([]byte, 0, len(prefix)+pixelBytes)
	chunked = append(chunked, prefix...)
	chunked = append(chunked, filtered...)

	probe := jpegProbeEncoder.EncodeAll(chunked, make([]byte, 0, len(chunked)/2))
	if len(probe)+len(suffix) >= len(orig) {
		return nil, false
	}
	return &BMPUnwrapped{
		Chunked: chunked,
		Recipe: &BMPRecipe{
			PrefixLen: dataOff, Suffix: suffix,
			RowBytes: rowBytes, Rows: rows, Step: step, Filters: filters,
		},
	}, true
}

// ReconstructBMP はレシピとチャンク化内容から元の BMP をビット単位で戻す。
func ReconstructBMP(recipe *BMPRecipe, chunked []byte) ([]byte, error) {
	if recipe == nil || recipe.PrefixLen < 0 || recipe.PrefixLen > len(chunked) ||
		recipe.RowBytes <= 0 || recipe.Rows < 0 || recipe.Step < 1 || recipe.Step > 4 {
		return nil, errors.New("BMP レシピが不正です")
	}
	pixelBytes := recipe.RowBytes * recipe.Rows
	prefix := chunked[:recipe.PrefixLen]
	filtered := chunked[recipe.PrefixLen:]
	if len(filtered) != pixelBytes || len(recipe.Filters) != recipe.Rows {
		return nil, errors.New("BMP ペイロード長が不一致")
	}
	pix := make([]byte, pixelBytes)
	prevRow := make([]byte, recipe.RowBytes)
	for r := 0; r < recipe.Rows; r++ {
		dst := pix[r*recipe.RowBytes : (r+1)*recipe.RowBytes]
		src := filtered[r*recipe.RowBytes : (r+1)*recipe.RowBytes]
		if err := unapplyRowFilter(recipe.Filters[r], src, prevRow, recipe.Step, dst); err != nil {
			return nil, err
		}
		prevRow = dst
	}
	out := make([]byte, 0, len(prefix)+pixelBytes+len(recipe.Suffix))
	out = append(out, prefix...)
	out = append(out, pix...)
	out = append(out, recipe.Suffix...)
	return out, nil
}

// ---- PNG 型の行フィルタ(0=None 1=Sub 2=Up 3=Average 4=Paeth) ----

func bestRowFilter(row, prev []byte, step int, tmp []byte) uint8 {
	var best uint8
	bestCost := ^uint64(0)
	for ft := uint8(0); ft <= 4; ft++ {
		applyRowFilter(ft, row, prev, step, tmp)
		var cost uint64
		for _, v := range tmp {
			// 符号付き振幅(小さいほど圧縮しやすい)で評価
			if v >= 128 {
				cost += uint64(256 - int(v))
			} else {
				cost += uint64(v)
			}
		}
		if cost < bestCost {
			bestCost, best = cost, ft
		}
	}
	return best
}

func applyRowFilter(ft uint8, row, prev []byte, step int, dst []byte) {
	for i := range row {
		var a, b, c int
		if i >= step {
			a = int(row[i-step])
			c = int(prev[i-step])
		}
		b = int(prev[i])
		var pred int
		switch ft {
		case 1:
			pred = a
		case 2:
			pred = b
		case 3:
			pred = (a + b) / 2
		case 4:
			pred = paeth(a, b, c)
		}
		dst[i] = byte(int(row[i]) - pred)
	}
}

func unapplyRowFilter(ft uint8, src, prev []byte, step int, dst []byte) error {
	if ft > 4 {
		return errors.New("BMP フィルタ種別が不正")
	}
	for i := range src {
		var a, b, c int
		if i >= step {
			a = int(dst[i-step]) // 復元済みの左
			c = int(prev[i-step])
		}
		b = int(prev[i])
		var pred int
		switch ft {
		case 1:
			pred = a
		case 2:
			pred = b
		case 3:
			pred = (a + b) / 2
		case 4:
			pred = paeth(a, b, c)
		}
		dst[i] = byte(int(src[i]) + pred)
	}
	return nil
}

// paeth は PNG の Paeth 予測子。
func paeth(a, b, c int) int {
	p := a + b - c
	pa, pb, pc := absInt(p-a), absInt(p-b), absInt(p-c)
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
