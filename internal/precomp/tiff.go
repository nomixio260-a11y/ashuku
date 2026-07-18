package precomp

// TIFF(非圧縮)の可逆分解。
//
// 非圧縮 TIFF(Compression=1)のストリップも BMP と同じく生ピクセルが並ぶ
// だけなので、BMP と共通の**行予測フィルタ**(None/Sub/Up/Average/Paeth を
// 行ごとに最小残差で選択、bmp.go)をストリップの各行にかけて残差にし、
// store の zstd/brotli に圧縮させる。スキャン文書・医用・GIS で使われる形式。
//
// ストリップは任意位置に散在しうるので、GIF/MJPEG と同じ「スケルトン+
// ペイロード」のセグメント方式で、各ストリップを原位置に戻す。可逆性は
// フィルタの全単射+ SHA-256 検証。純Go・cgo 不要。8bit・chunky のみ対応
// (それ以外・LZW/PackBits は安全に素通し)。

import (
	"encoding/binary"
	"errors"
	"sort"
)

// IsTIFF は TIFF シグネチャ(II* / MM*)を判定する。
func IsTIFF(head []byte) bool {
	if len(head) < 4 {
		return false
	}
	if head[0] == 'I' && head[1] == 'I' && head[2] == 0x2A && head[3] == 0x00 {
		return true
	}
	return head[0] == 'M' && head[1] == 'M' && head[2] == 0x00 && head[3] == 0x2A
}

// TIFFStrip は1ストリップの復元情報。
type TIFFStrip struct {
	Offset   int     `json:"o"`  // 原ファイル内の位置
	Length   int     `json:"l"`  // バイト長
	RowBytes int     `json:"rb"` // 1行のバイト数(TIFF はパディングなし)
	Rows     int     `json:"r"`
	Filters  []uint8 `json:"f"`
}

// TIFFRecipe は TIFF 再構成レシピ。チャンク化内容 = スケルトン(ストリップ
// 以外の原文全部)にストリップの位置でフィルタ済みバイトを挿入したもの。
type TIFFRecipe struct {
	Step   int         `json:"step"`
	Strips []TIFFStrip `json:"strips"`
}

// TIFFUnwrapped は分解結果。
type TIFFUnwrapped struct {
	Chunked []byte
	Recipe  *TIFFRecipe
}

const tiffMaxStrips = 65536

// TryUnwrapTIFF は非圧縮 TIFF のストリップに行フィルタをかける。
func TryUnwrapTIFF(orig []byte, maxPlain int64) (*TIFFUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if len(orig) < 16 || !IsTIFF(orig) {
		return nil, false
	}
	bo := binary.ByteOrder(binary.LittleEndian)
	if orig[0] == 'M' {
		bo = binary.BigEndian
	}
	ifdOff := int(bo.Uint32(orig[4:]))
	tags, ok := readIFD(orig, bo, ifdOff)
	if !ok {
		return nil, false
	}
	// 必須タグ
	comp := tagFirst(tags, 259, 1)
	if comp != 1 { // 非圧縮のみ
		return nil, false
	}
	width := int(tagFirst(tags, 256, 0))
	height := int(tagFirst(tags, 257, 0))
	spp := int(tagFirst(tags, 277, 1))
	planar := tagFirst(tags, 284, 1)
	if width <= 0 || height <= 0 || spp < 1 || spp > 4 || planar != 1 {
		return nil, false
	}
	// BitsPerSample はすべて 8 のみ対応
	bps := tags[258]
	if len(bps) == 0 {
		bps = []uint32{1}
	}
	for _, b := range bps {
		if b != 8 {
			return nil, false
		}
	}
	stripOffs := tags[273]
	stripLens := tags[279]
	if len(stripOffs) == 0 || len(stripOffs) != len(stripLens) || len(stripOffs) > tiffMaxStrips {
		return nil, false
	}
	rowsPerStrip := int(tagFirst(tags, 278, uint32(height)))
	if rowsPerStrip <= 0 {
		return nil, false
	}
	step := spp // 8bit なので step = サンプル数
	rowBytes := width * spp

	// ストリップ情報を作る(オフセット順にソートしてセグメント化)
	type strip struct{ off, length, rows int }
	strips := make([]strip, 0, len(stripOffs))
	var plainTotal int64
	for i := range stripOffs {
		off := int(stripOffs[i])
		length := int(stripLens[i])
		if off < 8 || length <= 0 || off+length > len(orig) {
			return nil, false
		}
		if rowBytes == 0 || length%rowBytes != 0 {
			return nil, false // 行境界に一致しない → 素通し
		}
		rows := length / rowBytes
		if rows <= 0 {
			return nil, false
		}
		plainTotal += int64(length)
		strips = append(strips, strip{off, length, rows})
	}
	if plainTotal > maxPlain || plainTotal < 512 {
		return nil, false
	}
	sort.Slice(strips, func(i, j int) bool { return strips[i].off < strips[j].off })
	// 重なり検査
	for i := 1; i < len(strips); i++ {
		if strips[i].off < strips[i-1].off+strips[i-1].length {
			return nil, false
		}
	}

	// セグメント化: スケルトンの隙間 + フィルタ済みストリップ
	var chunked []byte
	recStrips := make([]TIFFStrip, 0, len(strips))
	cursor := 0
	prevRow := make([]byte, rowBytes)
	tmp := make([]byte, rowBytes)
	for _, st := range strips {
		chunked = append(chunked, orig[cursor:st.off]...)
		pix := orig[st.off : st.off+st.length]
		filtered := make([]byte, st.length)
		filters := make([]uint8, st.rows)
		for i := range prevRow {
			prevRow[i] = 0 // ストリップごとに上端はゼロ
		}
		for r := 0; r < st.rows; r++ {
			row := pix[r*rowBytes : (r+1)*rowBytes]
			ft := bestRowFilter(row, prevRow, step, tmp)
			filters[r] = ft
			applyRowFilter(ft, row, prevRow, step, filtered[r*rowBytes:(r+1)*rowBytes])
			copy(prevRow, row)
		}
		chunked = append(chunked, filtered...)
		recStrips = append(recStrips, TIFFStrip{
			Offset: st.off, Length: st.length, RowBytes: rowBytes, Rows: st.rows, Filters: filters,
		})
		cursor = st.off + st.length
	}
	chunked = append(chunked, orig[cursor:]...)

	probe := jpegProbeEncoder.EncodeAll(chunked, make([]byte, 0, len(chunked)/2))
	if len(probe) >= len(orig) {
		return nil, false
	}
	return &TIFFUnwrapped{
		Chunked: chunked,
		Recipe:  &TIFFRecipe{Step: step, Strips: recStrips},
	}, true
}

// ReconstructTIFF はレシピとチャンク化内容から元の TIFF をビット単位で戻す。
func ReconstructTIFF(recipe *TIFFRecipe, chunked []byte) ([]byte, error) {
	if recipe == nil || recipe.Step < 1 || recipe.Step > 4 {
		return nil, errors.New("TIFF レシピが不正です")
	}
	// チャンク化内容 = 交互の [スケルトン][ストリップ]。ストリップは Recipe の
	// 順(=オフセット昇順)で並ぶ。各ストリップの原オフセットへ復元する。
	out := make([]byte, 0, len(chunked))
	cpos := 0 // chunked 内の読み取り位置
	opos := 0 // 復元中の出力オフセット
	for _, st := range recipe.Strips {
		if st.Offset < opos || st.RowBytes <= 0 || st.Rows < 0 || len(st.Filters) != st.Rows ||
			st.Length != st.RowBytes*st.Rows {
			return nil, errors.New("TIFF ストリップ情報が不正")
		}
		skelLen := st.Offset - opos
		if cpos+skelLen+st.Length > len(chunked) {
			return nil, errors.New("TIFF レシピが範囲外です")
		}
		out = append(out, chunked[cpos:cpos+skelLen]...)
		cpos += skelLen
		opos += skelLen
		// フィルタ解除
		filtered := chunked[cpos : cpos+st.Length]
		cpos += st.Length
		prevRow := make([]byte, st.RowBytes)
		for r := 0; r < st.Rows; r++ {
			dst := make([]byte, st.RowBytes)
			src := filtered[r*st.RowBytes : (r+1)*st.RowBytes]
			if err := unapplyRowFilter(st.Filters[r], src, prevRow, recipe.Step, dst); err != nil {
				return nil, err
			}
			out = append(out, dst...)
			prevRow = dst
		}
		opos += st.Length
	}
	// 末尾スケルトン
	out = append(out, chunked[cpos:]...)
	return out, nil
}

// readIFD は最初の IFD のタグを tag→値配列(SHORT/LONG)に読む。
func readIFD(d []byte, bo binary.ByteOrder, ifdOff int) (map[int][]uint32, bool) {
	if ifdOff < 8 || ifdOff+2 > len(d) {
		return nil, false
	}
	n := int(bo.Uint16(d[ifdOff:]))
	if n <= 0 || ifdOff+2+n*12 > len(d) {
		return nil, false
	}
	tags := make(map[int][]uint32, n)
	for i := 0; i < n; i++ {
		e := ifdOff + 2 + i*12
		tag := int(bo.Uint16(d[e:]))
		typ := int(bo.Uint16(d[e+2:]))
		cnt := int(bo.Uint32(d[e+4:]))
		if cnt < 0 || cnt > 1<<24 {
			return nil, false
		}
		var tsz int
		switch typ {
		case 3: // SHORT
			tsz = 2
		case 4: // LONG
			tsz = 4
		default:
			continue // 他の型はこの用途では不要
		}
		total := tsz * cnt
		valPos := e + 8
		if total > 4 {
			valPos = int(bo.Uint32(d[e+8:]))
		}
		if valPos < 0 || valPos+total > len(d) {
			return nil, false
		}
		vals := make([]uint32, cnt)
		for k := 0; k < cnt; k++ {
			if tsz == 2 {
				vals[k] = uint32(bo.Uint16(d[valPos+k*2:]))
			} else {
				vals[k] = bo.Uint32(d[valPos+k*4:])
			}
		}
		tags[tag] = vals
	}
	return tags, true
}

func tagFirst(tags map[int][]uint32, tag int, def uint32) uint32 {
	if v, ok := tags[tag]; ok && len(v) > 0 {
		return v[0]
	}
	return def
}
