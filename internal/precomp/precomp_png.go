//go:build cgo

package precomp

// PNG コンテナの precompression。
//
// PNG は「8バイト署名 + チャンク列(長さ+型+データ+CRC32)」で、画像本体は
// 連続する IDAT チャンクのデータ部を連結した1本の zlib ストリーム
// (フィルタ済みスキャンライン)。libpng 系はほぼ例外なく zlib で圧縮する
// ため、既存の zlib レベル探索でビット一致再現できる。
//
// 分解: [prefix(署名〜最初のIDAT直前)] + [IDATペイロード連結 = zlib] +
// [suffix(最後のIDAT直後〜EOF)] に切り、zlib を展開データ+レベルに分解。
// IDAT の分割位置(各ペイロード長)をレシピに記録する。
// 再構成: 展開データを再 deflate → 記録した長さで IDAT 列に分割し、
// 各チャンクの CRC32 を計算して組み立てる(ビット一致は保存時に検証)。
//
// 展開データ(フィルタ済みスキャンライン)は deflate より zstd-19 の方が
// よく縮み、類似 PNG 同士では dedup/デルタも効くようになる。

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// maxIDATChunks は1ファイルで扱う IDAT チャンク数の上限。
const maxIDATChunks = 4096

// IsPNG は PNG 署名(先頭3バイトで判定可能な範囲)かを返す。
func IsPNG(head []byte) bool {
	return len(head) >= 3 && head[0] == 0x89 && head[1] == 'P' && head[2] == 'N'
}

// PNGRecipe は PNG 再構成レシピ。
type PNGRecipe struct {
	// Prefix は署名から最初の IDAT チャンク直前までの原文。
	Prefix []byte `json:"prefix"`
	// Suffix は最後の IDAT チャンク直後から EOF までの原文。
	Suffix []byte `json:"suffix"`
	// IDATLens は各 IDAT チャンクのデータ部の長さ(分割位置の復元用)。
	IDATLens []uint32 `json:"idat_lens"`
	// ZHeader は zlib ヘッダ原文(2バイト)。
	ZHeader []byte `json:"zheader"`
}

// PNGUnwrapped は PNG を分解した結果。
type PNGUnwrapped struct {
	Recipe *PNGRecipe
	Plain  []byte // フィルタ済みスキャンライン(zlib の展開データ)
	Level  int
}

// TryUnwrapPNG は PNG を「展開データ+レシピ」に分解する。
// 全 IDAT の zlib が本家 zlib 産でビット一致再現できる場合のみ成功する。
func TryUnwrapPNG(orig []byte, maxPlain int64) (*PNGUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if len(orig) < len(pngSignature)+12 || !bytes.HasPrefix(orig, pngSignature) {
		return nil, false
	}

	// チャンク走査で IDAT の連続領域を特定する
	pos := len(pngSignature)
	firstIDAT, afterIDAT := -1, -1
	var lens []uint32
	var stream []byte
	for pos+12 <= len(orig) {
		dataLen := int(binary.BigEndian.Uint32(orig[pos:]))
		typ := string(orig[pos+4 : pos+8])
		end := pos + 12 + dataLen
		if dataLen > len(orig) || end > len(orig) {
			return nil, false // 壊れた構造
		}
		if typ == "IDAT" {
			if afterIDAT != -1 {
				return nil, false // IDAT が非連続(仕様違反)
			}
			if firstIDAT == -1 {
				firstIDAT = pos
			}
			if len(lens) >= maxIDATChunks {
				return nil, false
			}
			lens = append(lens, uint32(dataLen))
			// チャンク = 長さ(4) + 型(4) + データ + CRC(4)。データは pos+8 から。
			stream = append(stream, orig[pos+8:pos+8+dataLen]...)
		} else if firstIDAT != -1 && afterIDAT == -1 {
			afterIDAT = pos
		}
		pos = end
		if typ == "IEND" {
			break
		}
	}
	if firstIDAT == -1 || pos > len(orig) {
		return nil, false
	}
	if afterIDAT == -1 {
		afterIDAT = pos
	}
	if len(stream) < 6 || !IsZlib(stream) {
		return nil, false
	}

	// 連結ペイロード = 生 zlib ストリームとして既存機構で分解
	u, ok := TryUnwrapZlib(stream, maxPlain)
	if !ok {
		return nil, false
	}
	recipe := &PNGRecipe{
		Prefix:   append([]byte(nil), orig[:firstIDAT]...),
		Suffix:   append([]byte(nil), orig[afterIDAT:]...),
		IDATLens: lens,
		ZHeader:  u.Header,
	}
	return &PNGUnwrapped{Recipe: recipe, Plain: u.Plain, Level: u.Level}, true
}

// ReconstructPNG はレシピから元の PNG をビット単位で再構成する。
func ReconstructPNG(recipe *PNGRecipe, level int, plain []byte) ([]byte, error) {
	stream, err := ReconstructZlib(recipe.ZHeader, level, plain)
	if err != nil {
		return nil, err
	}
	total := 0
	for _, l := range recipe.IDATLens {
		total += int(l)
	}
	if total != len(stream) {
		return nil, fmt.Errorf("IDAT 長の合計(%d)が zlib ストリーム長(%d)と一致しません", total, len(stream))
	}
	out := make([]byte, 0, len(recipe.Prefix)+len(stream)+12*len(recipe.IDATLens)+len(recipe.Suffix))
	out = append(out, recipe.Prefix...)
	off := 0
	var buf4 [4]byte
	for _, l := range recipe.IDATLens {
		payload := stream[off : off+int(l)]
		off += int(l)
		binary.BigEndian.PutUint32(buf4[:], l)
		out = append(out, buf4[:]...)
		out = append(out, "IDAT"...)
		out = append(out, payload...)
		crc := crc32.NewIEEE()
		crc.Write([]byte("IDAT"))
		crc.Write(payload)
		binary.BigEndian.PutUint32(buf4[:], crc.Sum32())
		out = append(out, buf4[:]...)
	}
	return append(out, recipe.Suffix...), nil
}
