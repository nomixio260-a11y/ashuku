package precomp

// GIF の可逆分解(LZW アンラップ)。
//
// GIF の画像データは LZW 圧縮されているが、LZW は 1987 年の方式で現代の
// エントロピー圧縮より大幅に弱い(実測: 生ピクセルを zstd 最高レベルで
// 圧縮すると LZW の 20% 程度になる)。そこで LZW を復号して生ピクセル列に
// 戻し、「スケルトン+ピクセル列」としてチャンク化する。生ピクセルには
// 重複排除・類似デルタ・zstd/brotli が効く(近似フレーム同士のアニメ GIF
// では dedup も効く)。
//
// 可逆性の保証: 復号したピクセルを compress/lzw で再符号化し、サブブロック
// 分割まで含めて**元のバイト列とビット一致する画像だけ**をアンラップする
// (Go/giflib 系エンコーダ産は一致する。一致しないエンコーダ産は素通しで
// 安全)。復元時は SHA-256 の最終検証もかかる(store 層)。純Go・cgo 不要。

import (
	"bytes"
	"compress/lzw"
	"errors"
	"io"
)

// IsGIF は GIF シグネチャ(GIF87a/GIF89a の先頭4バイト)を判定する。
func IsGIF(head []byte) bool {
	return len(head) >= 4 && head[0] == 'G' && head[1] == 'I' && head[2] == 'F' && head[3] == '8'
}

// GIFSegment はチャンク化ストリームの1区間:
// スケルトン原文 SkelLen バイト + 生ピクセル PixelLen バイト。
// PixelLen==0 の場合は終端スケルトンのみ。
type GIFSegment struct {
	SkelLen  int64 `json:"s"`
	PixelLen int64 `json:"p,omitempty"`
	// MinCode は LZW の最小コード幅(画像ごとの1バイト)。
	MinCode int `json:"m,omitempty"`
	// Blocks は元のサブブロック長列(再符号化バイト列をこの長さで分割して
	// 復元する。長さの合計 = LZW ストリーム長)。
	Blocks []int `json:"b,omitempty"`
}

// GIFRecipe は GIF 再構成レシピ。
type GIFRecipe struct {
	Segments []GIFSegment `json:"segs"`
}

// GIFUnwrapped は分解結果。
type GIFUnwrapped struct {
	Chunked []byte
	Recipe  *GIFRecipe
}

// gifMaxImages はパースする画像ブロック数の上限(暴走防止)。
const gifMaxImages = 4096

// TryUnwrapGIF は GIF を「スケルトン+生ピクセル列」に分解する。
// ビット一致で再符号化できる画像だけをアンラップし(部分適用)、
// 1画像もアンラップできない・見込みサイズが元以上なら不採用。
func TryUnwrapGIF(orig []byte, maxPlain int64) (*GIFUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if len(orig) < 32 || !IsGIF(orig) || len(orig) < 13 {
		return nil, false
	}
	p := 13
	// グローバルカラーテーブル
	if orig[10]&0x80 != 0 {
		p += 3 * (1 << ((orig[10] & 7) + 1))
	}
	if p >= len(orig) {
		return nil, false
	}

	var chunked []byte
	var segs []GIFSegment
	skelStart := 0 // 現在のスケルトン区間の開始位置(orig 内)
	var plainTotal int64
	unwrapped := 0
	images := 0

	cut := func(skelEnd int, pixels []byte, minCode int, blocks []int) {
		chunked = append(chunked, orig[skelStart:skelEnd]...)
		chunked = append(chunked, pixels...)
		segs = append(segs, GIFSegment{
			SkelLen:  int64(skelEnd - skelStart),
			PixelLen: int64(len(pixels)),
			MinCode:  minCode,
			Blocks:   blocks,
		})
	}

loop:
	for p < len(orig) {
		switch orig[p] {
		case 0x3B: // trailer
			p++
			break loop
		case 0x21: // 拡張ブロック
			p += 2
			for p < len(orig) && orig[p] != 0 {
				p += int(orig[p]) + 1
			}
			if p >= len(orig) {
				return nil, false
			}
			p++ // terminator
		case 0x2C: // 画像
			images++
			if images > gifMaxImages {
				return nil, false
			}
			if p+10 > len(orig) {
				return nil, false
			}
			hp := p
			p += 10
			// ローカルカラーテーブル
			if orig[hp+9]&0x80 != 0 {
				p += 3 * (1 << ((orig[hp+9] & 7) + 1))
			}
			if p >= len(orig) {
				return nil, false
			}
			minCode := int(orig[p])
			p++
			if minCode < 2 || minCode > 8 {
				return nil, false
			}
			// サブブロック列を集める
			dataStart := p
			var lzwData []byte
			var blocks []int
			for p < len(orig) && orig[p] != 0 {
				n := int(orig[p])
				if p+1+n > len(orig) {
					return nil, false
				}
				blocks = append(blocks, n)
				lzwData = append(lzwData, orig[p+1:p+1+n]...)
				p += n + 1
			}
			if p >= len(orig) {
				return nil, false
			}
			p++ // terminator (0x00)
			dataEnd := p

			// 復号 → 再符号化 → サブブロック再構成でビット一致検証
			r := lzw.NewReader(bytes.NewReader(lzwData), lzw.LSB, minCode)
			pixels, err := io.ReadAll(io.LimitReader(r, maxPlain-plainTotal+1))
			r.Close()
			if err != nil || plainTotal+int64(len(pixels)) > maxPlain {
				continue // この画像は素通し(スケルトンに残る)
			}
			rebuilt, ok := gifRebuildSubBlocks(pixels, minCode, blocks)
			if !ok || !bytes.Equal(rebuilt, orig[dataStart:dataEnd]) {
				continue // ビット一致しないエンコーダ産 → 素通し
			}
			// 採用: スケルトンは minCode バイトの直後まで、続いて生ピクセル
			cut(dataStart, pixels, minCode, blocks)
			plainTotal += int64(len(pixels))
			skelStart = dataEnd
			unwrapped++
		default:
			return nil, false // 未知ブロック → GIF 全体を素通し
		}
	}
	if unwrapped == 0 {
		return nil, false
	}
	// 終端スケルトン(最後の画像以降の原文すべて)
	chunked = append(chunked, orig[skelStart:]...)
	segs = append(segs, GIFSegment{SkelLen: int64(len(orig) - skelStart)})

	// 見込み判定: チャンク化内容を zstd 最高レベルで probe し、元より
	// 小さくなる場合のみ採用(単画像で悪化させない)。
	probe := jpegProbeEncoder.EncodeAll(chunked, make([]byte, 0, len(chunked)/2))
	if len(probe) >= len(orig) {
		return nil, false
	}
	return &GIFUnwrapped{Chunked: chunked, Recipe: &GIFRecipe{Segments: segs}}, true
}

// gifRebuildSubBlocks はピクセルを LZW 再符号化し、blocks の長さ列で
// サブブロック分割して「サブブロック列+終端 0x00」のバイト列を返す。
// 再符号化長が blocks の合計と一致しない場合は失敗。
func gifRebuildSubBlocks(pixels []byte, minCode int, blocks []int) ([]byte, bool) {
	var enc bytes.Buffer
	w := lzw.NewWriter(&enc, lzw.LSB, minCode)
	if _, err := w.Write(pixels); err != nil {
		return nil, false
	}
	if err := w.Close(); err != nil {
		return nil, false
	}
	data := enc.Bytes()
	total := 0
	for _, n := range blocks {
		if n <= 0 || n > 255 {
			return nil, false
		}
		total += n
	}
	if total != len(data) {
		return nil, false
	}
	out := make([]byte, 0, len(data)+len(blocks)+1)
	off := 0
	for _, n := range blocks {
		out = append(out, byte(n))
		out = append(out, data[off:off+n]...)
		off += n
	}
	out = append(out, 0x00)
	return out, true
}

// ReconstructGIF はレシピとチャンク化内容から元の GIF をビット単位で戻す。
func ReconstructGIF(recipe *GIFRecipe, chunked []byte) ([]byte, error) {
	if recipe == nil {
		return nil, errors.New("GIF レシピが不正です")
	}
	out := make([]byte, 0, len(chunked))
	off := int64(0)
	for _, seg := range recipe.Segments {
		if seg.SkelLen < 0 || seg.PixelLen < 0 || off+seg.SkelLen+seg.PixelLen > int64(len(chunked)) {
			return nil, errors.New("GIF レシピが範囲外です")
		}
		out = append(out, chunked[off:off+seg.SkelLen]...)
		off += seg.SkelLen
		if seg.PixelLen == 0 {
			continue
		}
		pixels := chunked[off : off+seg.PixelLen]
		off += seg.PixelLen
		rebuilt, ok := gifRebuildSubBlocks(pixels, seg.MinCode, seg.Blocks)
		if !ok {
			return nil, errors.New("GIF LZW 再符号化が一致しません")
		}
		out = append(out, rebuilt...)
	}
	return out, nil
}
