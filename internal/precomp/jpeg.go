package precomp

// JPEG(baseline sequential DCT / Huffman)の可逆再圧縮。
//
// JPEG は既に DCT + 量子化 + Huffman 符号化されているため、外側から zstd を
// かけてもほぼ縮まない。しかし Huffman ストリームを復号して量子化 DCT 係数に
// 戻し、係数を「係数位置ごとの平面」に並べ替えると、高周波成分に集中する
// ゼロがまとまって zstd で大きく縮み、さらに似た画像(バースト撮影・再保存・
// サムネイル)同士でチャンク重複排除・デルタ圧縮が効くようになる
// (lepton / packJPG が使うのと同じ「係数を可逆に取り出す」原理の純Go版。
// エントロピー再符号化に算術符号ではなく zstd を使うぶん単画像の利得は
// 控えめだが、外部依存ゼロ・CGO 不要で動く)。
//
// 対応: baseline sequential DCT(SOF0)、Huffman 符号、8ビット精度、
// リスタートマーカ対応。プログレッシブ(SOF2)・算術符号・12ビットは対象外
// (分解を諦めて素通し = 元の JPEG のまま raw 保存される)。
//
// 可逆性: 復号した係数を標準アルゴリズムで再 Huffman 符号化し、元のバイト列と
// ビット一致することを保存前に検証する。一致しない(非標準エンコーダ産)場合は
// 採用しない。読み出し時も SHA-256 で最終検証する。
//
// 純 Go・cgo 非依存(CGO 無効ビルドでも JPEG 再圧縮は有効)。

import (
	"encoding/binary"
	"errors"
)

// IsJPEG は JPEG の SOI + マーカ開始かを判定する。
func IsJPEG(head []byte) bool {
	return len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF
}

// JPEG 係数コーダ種別(JPEGRecipe.Coder)。
const (
	jpegCoderPlanes = 0 // 係数平面 varint(store の zstd/dedup/デルタが効く)
	jpegCoderArith  = 1 // 文脈モデル+レンジ符号(単画像が最も縮む)
)

// JPEGRecipe は JPEG 再構成レシピ(チャンク化内容の分割情報)。
// チャンク化内容 = prefix(SOSヘッダまでの原文)+ ペイロード(Coder に応じて
// 係数平面 or 文脈算術符号)。
type JPEGRecipe struct {
	PrefixLen int    `json:"prefix_len"` // チャンク化内容の先頭を占めるヘッダ部の長さ
	Suffix    []byte `json:"suffix"`     // EOI 以降の原文(通常 FFD9 の2バイト)
	Coder     int    `json:"coder"`      // ペイロードの係数コーダ(planes | arith)
}

// JPEGUnwrapped は分解結果。
type JPEGUnwrapped struct {
	Chunked []byte // prefix || coefplanes(dedup/zstd の対象)
	Recipe  *JPEGRecipe
}

// jpeg マーカ
const (
	mSOI  = 0xD8
	mEOI  = 0xD9
	mSOS  = 0xDA
	mSOF0 = 0xC0
	mDHT  = 0xC4
	mDRI  = 0xDD
	mRST0 = 0xD0
	mRST7 = 0xD7
)

// component はスキャン成分の幾何。
type component struct {
	id       int
	h, v     int // サンプリング係数
	dcTable  int
	acTable  int
	blocksMC int // このMCU内のブロック数 = h*v
}

// jpegFrame は SOF/SOS から得た構造。
type jpegFrame struct {
	width, height int
	comps         []component
	restart       int // DRI(0=なし)
	hmax, vmax    int
	mcuX, mcuY    int
	dc            [4]*huffTable // DC テーブル(id 0..3)
	ac            [4]*huffTable // AC テーブル
}

// huffTable は Huffman 復号/符号化テーブル。
type huffTable struct {
	// 復号用: bits/vals から作った (code,length)->symbol。
	minCode [17]int32
	maxCode [17]int32 // -1 = 該当長なし
	valPtr  [17]int
	vals    []byte
	// 符号化用: symbol -> (code,length)
	encCode [256]uint16
	encSize [256]uint8
}

func buildHuff(bits []byte /*16*/, vals []byte) *huffTable {
	t := &huffTable{vals: vals}
	// canonical code 生成
	var huffsize []int
	for l := 1; l <= 16; l++ {
		for i := 0; i < int(bits[l-1]); i++ {
			huffsize = append(huffsize, l)
		}
	}
	codes := make([]uint16, len(huffsize))
	code := 0
	if len(huffsize) > 0 {
		si := huffsize[0]
		for k := 0; k < len(huffsize); {
			for k < len(huffsize) && huffsize[k] == si {
				codes[k] = uint16(code)
				code++
				k++
			}
			if k < len(huffsize) {
				for huffsize[k] != si {
					code <<= 1
					si++
				}
			}
		}
	}
	// 符号化テーブル
	for i, sz := range huffsize {
		v := vals[i]
		t.encCode[v] = codes[i]
		t.encSize[v] = uint8(sz)
	}
	// 復号テーブル(JPEG 附属書 F の mincode/maxcode/valptr)
	p := 0
	for l := 1; l <= 16; l++ {
		if bits[l-1] == 0 {
			t.maxCode[l] = -1
			continue
		}
		t.valPtr[l] = p
		t.minCode[l] = int32(codes[p])
		p += int(bits[l-1])
		t.maxCode[l] = int32(codes[p-1])
	}
	return t
}

// ---- ビットリーダ(エントロピー復号、FF00 除去・マーカ検出) ----
// バイト単位に精密に進める素朴な実装(pos が常に正確なので、リスタート
// マーカの境界処理が単純になる)。
type bitReader struct {
	data      []byte
	pos       int
	b         uint32
	n         int  // バッファ中の残りビット数
	hitMarker byte // マーカに到達したら非0
}

func (r *bitReader) readBit() int {
	if r.n == 0 {
		if r.pos >= len(r.data) {
			return 0 // パディング(1でも0でも、正しい復号では使われない)
		}
		c := r.data[r.pos]
		if c == 0xFF {
			nb := byte(0)
			if r.pos+1 < len(r.data) {
				nb = r.data[r.pos+1]
			}
			if nb == 0x00 {
				r.pos += 2 // スタッフィング解除(FF00 → FF)
			} else {
				r.hitMarker = nb
				return 0 // マーカは消費しない
			}
		} else {
			r.pos++
		}
		r.b = uint32(c)
		r.n = 8
	}
	r.n--
	return int((r.b >> uint(r.n)) & 1)
}

func (r *bitReader) readBits(n int) int {
	v := 0
	for i := 0; i < n; i++ {
		v = (v << 1) | r.readBit()
	}
	return v
}

// decodeHuff は1シンボルを復号する。
func (r *bitReader) decodeHuff(t *huffTable) (byte, error) {
	code := int32(0)
	for l := 1; l <= 16; l++ {
		code = (code << 1) | int32(r.readBit())
		if t.maxCode[l] >= 0 && code <= t.maxCode[l] {
			idx := t.valPtr[l] + int(code-t.minCode[l])
			if idx < 0 || idx >= len(t.vals) {
				return 0, errors.New("Huffman インデックス範囲外")
			}
			return t.vals[idx], nil
		}
	}
	return 0, errors.New("Huffman 符号が見つかりません")
}

// takeRestart はバイト境界のリスタートマーカ(FF D0..D7)を消費する。
func (r *bitReader) takeRestart() bool {
	r.n = 0 // 現在バイトの残り(パディング)を捨てる
	if r.pos+1 < len(r.data) && r.data[r.pos] == 0xFF &&
		r.data[r.pos+1] >= mRST0 && r.data[r.pos+1] <= mRST7 {
		r.pos += 2
		r.hitMarker = 0
		return true
	}
	return false
}

// extend は JPEG の可変長数値の符号拡張。
func extend(v, n int) int {
	if n == 0 {
		return 0
	}
	if v < (1 << (n - 1)) {
		return v - (1 << n) + 1
	}
	return v
}

// zigzag 順(JPEG 標準)。
var zigzag = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63,
}

// parseFrame は prefix(SOSヘッダまで)を解析して幾何とテーブルを得る。
// 返り値の entropyStart は元データ中でエントロピーが始まる位置。
func parseFrame(orig []byte) (*jpegFrame, int, error) {
	f := &jpegFrame{}
	i := 2 // SOI をスキップ
	for i+1 < len(orig) {
		if orig[i] != 0xFF {
			return nil, 0, errors.New("マーカが不正")
		}
		m := orig[i+1]
		i += 2
		if m == mSOI || m == mEOI {
			continue
		}
		if m >= mRST0 && m <= mRST7 {
			continue
		}
		if i+2 > len(orig) {
			return nil, 0, errors.New("セグメント長が不正")
		}
		segLen := int(binary.BigEndian.Uint16(orig[i:]))
		if segLen < 2 || i+segLen > len(orig) {
			return nil, 0, errors.New("セグメント長が範囲外")
		}
		seg := orig[i+2 : i+segLen]
		switch m {
		case mSOF0:
			if len(seg) < 6 {
				return nil, 0, errors.New("SOF0 が短い")
			}
			if seg[0] != 8 {
				return nil, 0, errors.New("8ビット精度のみ対応")
			}
			f.height = int(binary.BigEndian.Uint16(seg[1:]))
			f.width = int(binary.BigEndian.Uint16(seg[3:]))
			nc := int(seg[5])
			if nc < 1 || nc > 4 || len(seg) < 6+nc*3 {
				return nil, 0, errors.New("SOF0 成分数が不正")
			}
			for c := 0; c < nc; c++ {
				o := 6 + c*3
				comp := component{
					id: int(seg[o]),
					h:  int(seg[o+1] >> 4),
					v:  int(seg[o+1] & 0x0F),
				}
				if comp.h < 1 || comp.v < 1 || comp.h > 4 || comp.v > 4 {
					return nil, 0, errors.New("サンプリング係数が不正")
				}
				comp.blocksMC = comp.h * comp.v
				if comp.h > f.hmax {
					f.hmax = comp.h
				}
				if comp.v > f.vmax {
					f.vmax = comp.v
				}
				f.comps = append(f.comps, comp)
			}
		case mDHT:
			p := 0
			for p < len(seg) {
				if p+17 > len(seg) {
					return nil, 0, errors.New("DHT が短い")
				}
				tc := seg[p] >> 4
				th := seg[p] & 0x0F
				if th > 3 {
					return nil, 0, errors.New("Huffman テーブルID不正")
				}
				bits := seg[p+1 : p+17]
				total := 0
				for _, b := range bits {
					total += int(b)
				}
				if p+17+total > len(seg) {
					return nil, 0, errors.New("DHT vals が範囲外")
				}
				vals := append([]byte(nil), seg[p+17:p+17+total]...)
				ht := buildHuff(bits, vals)
				if tc == 0 {
					f.dc[th] = ht
				} else {
					f.ac[th] = ht
				}
				p += 17 + total
			}
		case mDRI:
			if len(seg) >= 2 {
				f.restart = int(binary.BigEndian.Uint16(seg))
			}
		case mSOS:
			if len(seg) < 1 {
				return nil, 0, errors.New("SOS が短い")
			}
			ns := int(seg[0])
			if len(seg) < 1+ns*2+3 {
				return nil, 0, errors.New("SOS が短い")
			}
			// スキャン成分の DC/AC テーブル割り当て(SOF の成分順に対応付け)
			for s := 0; s < ns; s++ {
				cid := int(seg[1+s*2])
				td := int(seg[2+s*2] >> 4)
				ta := int(seg[2+s*2] & 0x0F)
				for ci := range f.comps {
					if f.comps[ci].id == cid {
						f.comps[ci].dcTable = td
						f.comps[ci].acTable = ta
					}
				}
			}
			so := 1 + ns*2
			ss, se := seg[so], seg[so+1]
			ahal := seg[so+2]
			if ss != 0 || se != 63 || ahal != 0 {
				return nil, 0, errors.New("baseline(Ss=0,Se=63)のみ対応")
			}
			if ns != len(f.comps) {
				return nil, 0, errors.New("非インターリーブは未対応")
			}
			entropyStart := i + segLen
			f.mcuX = (f.width + 8*f.hmax - 1) / (8 * f.hmax)
			f.mcuY = (f.height + 8*f.vmax - 1) / (8 * f.vmax)
			return f, entropyStart, nil
		}
		i += segLen
	}
	return nil, 0, errors.New("SOS が見つかりません")
}
