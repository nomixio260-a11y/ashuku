//go:build cgo

// Package precomp は gzip ストリームの可逆 precompression を提供する。
//
// gzip(zlib 産)の中身は「zlib の deflate をレベル等のパラメータで再実行
// すればビット単位で再現できる」ことが知られている(precomp / AntiZ が
// 実証した方式)。そこで gzip を「展開データ + 再構成レシピ(ヘッダ原文と
// 圧縮レベル)」に分解して保存し、展開データを重複排除・類似デルタ・zstd の
// 対象にする。復元時はレシピで deflate し直して元のバイト列を組み立てる。
//
// 分解は保存時にビット一致を検証してからしか採用しない(一致しない
// ストリーム = zlib 以外の実装が生成したものは、素通しで通常保存される)。
// 再構成はシステムの zlib に依存するため、zlib のバージョン更新で出力が
// 変わった場合は復元時のハッシュ検証が失敗し、エラーが返る(壊れたデータを
// 返すことはない)。
package precomp

/*
#cgo LDFLAGS: -lz
#include <zlib.h>
#include <stdlib.h>
#include <string.h>

// raw deflate (windowBits=-15) を memLevel / strategy 指定つきで1回実行する。
// 戻り値: 出力長(>=0)、エラー時は負の zlib エラーコード。
static long deflate_exact(const unsigned char* in, unsigned long in_len,
                          unsigned char* out, unsigned long out_cap,
                          int level, int memLevel, int strategy) {
	z_stream zs;
	memset(&zs, 0, sizeof(zs));
	int rc = deflateInit2(&zs, level, Z_DEFLATED, -15, memLevel, strategy);
	if (rc != Z_OK) return rc < 0 ? rc : -100;
	zs.next_in = (unsigned char*)in;
	zs.avail_in = in_len;
	zs.next_out = out;
	zs.avail_out = out_cap;
	rc = deflate(&zs, Z_FINISH);
	long n = (long)(out_cap - zs.avail_out);
	deflateEnd(&zs);
	if (rc != Z_STREAM_END) return rc < 0 ? rc : -101;
	return n;
}

static unsigned long deflate_bound(unsigned long n, int memLevel) {
	z_stream zs;
	memset(&zs, 0, sizeof(zs));
	if (deflateInit2(&zs, 6, Z_DEFLATED, -15, memLevel, Z_DEFAULT_STRATEGY) != Z_OK) return n + n/2 + 1024;
	unsigned long b = deflateBound(&zs, n);
	deflateEnd(&zs);
	// memLevel 次第で保存ブロックが増えうるので余裕を持たせる
	return b + n/8 + 1024;
}
*/
import "C"

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"unsafe"
)

// Supported はこのビルドで precompression が使えるかを返す。
func Supported() bool { return true }

// Unwrapped は gzip を分解した結果。
type Unwrapped struct {
	// Header は gzip ヘッダの原文(FNAME 等の可変部を含む)。
	Header []byte
	// Plain は展開データ。
	Plain []byte
	// Level は再構成に使う zlib 圧縮レベル。
	Level int
}

// gzip ヘッダのフラグビット
const (
	flagHCRC    = 1 << 1
	flagExtra   = 1 << 2
	flagName    = 1 << 3
	flagComment = 1 << 4
)

// IsGzip は gzip マジック(単一メンバー deflate)かを判定する。
func IsGzip(head []byte) bool {
	return len(head) >= 3 && head[0] == 0x1f && head[1] == 0x8b && head[2] == 8
}

// parseHeaderLen は gzip ヘッダの長さを返す。
func parseHeaderLen(data []byte) (int, error) {
	if len(data) < 10 {
		return 0, fmt.Errorf("gzip ヘッダが短すぎます")
	}
	flags := data[3]
	n := 10
	if flags&flagExtra != 0 {
		if len(data) < n+2 {
			return 0, io.ErrUnexpectedEOF
		}
		n += 2 + int(binary.LittleEndian.Uint16(data[n:]))
		if n > len(data) {
			// XLEN がデータ長を超えている(壊れた/細工されたヘッダ)。
			// ここで止めないと後続の data[n:] が範囲外になる(fuzz で検出)。
			return 0, io.ErrUnexpectedEOF
		}
	}
	for _, f := range []byte{flagName, flagComment} {
		if flags&f == 0 {
			continue
		}
		i := bytes.IndexByte(data[n:], 0)
		if i < 0 {
			return 0, io.ErrUnexpectedEOF
		}
		n += i + 1
	}
	if flags&flagHCRC != 0 {
		n += 2
	}
	if n > len(data) {
		return 0, io.ErrUnexpectedEOF
	}
	return n, nil
}

// パラメータ符号化: レシピの「レベル」フィールド(int)に level / memLevel /
// strategy を詰める。0..9 は従来形式(level のみ、memLevel=8・default
// strategy)と解釈するため、既存レシピとの後方互換が保たれる。
func encodeParams(level, memLevel, strategy int) int {
	if memLevel == 8 && strategy == 0 {
		return level
	}
	return level | memLevel<<8 | strategy<<16
}

func decodeParams(v int) (level, memLevel, strategy int) {
	if v >= 0 && v <= 9 {
		return v, 8, 0
	}
	return v & 0xff, (v >> 8) & 0xff, (v >> 16) & 0xff
}

// deflateExact は zlib の raw deflate をパラメータ指定で実行する。
// params は encodeParams の符号化値(0..9 は素のレベル)。
func deflateExact(plain []byte, params int) ([]byte, error) {
	level, memLevel, strategy := decodeParams(params)
	bound := uint64(C.deflate_bound(C.ulong(len(plain)), C.int(memLevel)))
	out := make([]byte, bound)
	var inPtr *C.uchar
	if len(plain) > 0 {
		inPtr = (*C.uchar)(unsafe.Pointer(&plain[0]))
	}
	n := C.deflate_exact(inPtr, C.ulong(len(plain)),
		(*C.uchar)(unsafe.Pointer(&out[0])), C.ulong(bound),
		C.int(level), C.int(memLevel), C.int(strategy))
	if n < 0 {
		return nil, fmt.Errorf("zlib deflate 失敗 (code %d)", int(n))
	}
	return out[:n], nil
}

// 探索候補。標準構成(memLevel=8, default strategy)のレベル 6/9 が実世界の
// 大半を占めるので先に試し、外れたら拡張構成(memLevel 9 = 一部ライブラリの
// 設定、Z_FILTERED = PNG 最適化ツール等)を試す。
// Z_HUFFMAN_ONLY / Z_RLE をストリーム全体に使うプロデューサは実世界では
// 稀なため探索に含めない(RESEARCH.md §4.15)。
var paramOrder, paramOrderSmall = func() ([]int, []int) {
	var full, small []int
	levels := []int{6, 9, 1, 2, 3, 4, 5, 7, 8}
	// 標準構成(従来の探索空間)
	for _, l := range levels {
		full = append(full, encodeParams(l, 8, 0))
	}
	small = append(small, full...)
	// 拡張: memLevel 9(zlib 利用側が MAX_MEM_LEVEL を指定するケース)
	for _, l := range levels {
		full = append(full, encodeParams(l, 9, 0))
	}
	// 拡張: Z_FILTERED(PNG 系ツールが使う)× 標準/最大 memLevel
	for _, l := range levels {
		full = append(full, encodeParams(l, 8, 1))
	}
	// 大きな入力用の縮小版: 拡張はよく現れる組み合わせだけに絞る
	small = append(small,
		encodeParams(6, 9, 0), encodeParams(9, 9, 0),
		encodeParams(6, 8, 1), encodeParams(9, 8, 1))
	return full, small
}()

// fullSearchMax を超える展開データでは縮小候補だけ試す
// (1候補=1回の deflate なので、巨大入力での全探索は CPU を浪費する)。
const fullSearchMax = 8 << 20

// findLevel は deflate ストリームをビット一致再現できる zlib パラメータを
// 探す。返り値は encodeParams の符号化値(標準構成なら素のレベル)。
func findLevel(plain, deflateStream []byte) (int, bool) {
	order := paramOrder
	if len(plain) > fullSearchMax {
		order = paramOrderSmall
	}
	for _, params := range order {
		candidate, err := deflateExact(plain, params)
		if err != nil {
			return 0, false
		}
		if bytes.Equal(candidate, deflateStream) {
			return params, true
		}
	}
	return 0, false
}

// TryUnwrap は gzip ストリームを「展開データ+レシピ」に分解する。
// ビット一致で再構成できる場合のみ結果を返す(それ以外は ok=false)。
// maxPlain は展開データの上限(0 ならデフォルト上限)で、超えると分解を
// 諦める(zip bomb・メモリ対策)。
func TryUnwrap(orig []byte, maxPlain int64) (*Unwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if !IsGzip(orig) || len(orig) < 18 {
		return nil, false
	}
	headerLen, err := parseHeaderLen(orig)
	if err != nil {
		return nil, false
	}
	if len(orig) < headerLen+8 {
		return nil, false
	}
	deflateStream := orig[headerLen : len(orig)-8]
	trailer := orig[len(orig)-8:]

	// 展開(デコードは実装非依存なので Go 標準の flate でよい)
	fr := flate.NewReader(bytes.NewReader(deflateStream))
	plain, err := io.ReadAll(io.LimitReader(fr, maxPlain+1))
	fr.Close()
	if err != nil || int64(len(plain)) > maxPlain {
		return nil, false // マルチメンバー・上限超過等はここで弾かれる
	}
	// トレーラ検証(CRC32 + ISIZE)
	if crc32.ChecksumIEEE(plain) != binary.LittleEndian.Uint32(trailer) ||
		uint32(len(plain)) != binary.LittleEndian.Uint32(trailer[4:]) {
		return nil, false
	}

	// レベル探索: zlib で再圧縮してビット一致するレベルを探す
	if level, ok := findLevel(plain, deflateStream); ok {
		header := append([]byte(nil), orig[:headerLen]...)
		return &Unwrapped{Header: header, Plain: plain, Level: level}, true
	}
	return nil, false // zlib 産ではない(GNU gzip / zopfli / Go 等)
}

// Reconstruct はレシピから元の gzip バイト列をビット単位で再構成する。
func Reconstruct(header []byte, level int, plain []byte) ([]byte, error) {
	stream, err := deflateExact(plain, level)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(header)+len(stream)+8)
	out = append(out, header...)
	out = append(out, stream...)
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[:], crc32.ChecksumIEEE(plain))
	binary.LittleEndian.PutUint32(trailer[4:], uint32(len(plain)))
	return append(out, trailer[:]...), nil
}
