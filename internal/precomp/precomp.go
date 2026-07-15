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

// raw deflate (windowBits=-15, memLevel=8, default strategy) を1回で実行する。
// 戻り値: 出力長(>=0)、エラー時は負の zlib エラーコード。
static long deflate_exact(const unsigned char* in, unsigned long in_len,
                          unsigned char* out, unsigned long out_cap, int level) {
	z_stream zs;
	memset(&zs, 0, sizeof(zs));
	int rc = deflateInit2(&zs, level, Z_DEFLATED, -15, 8, Z_DEFAULT_STRATEGY);
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

static unsigned long deflate_bound(unsigned long n) {
	z_stream zs;
	memset(&zs, 0, sizeof(zs));
	if (deflateInit2(&zs, 6, Z_DEFLATED, -15, 8, Z_DEFAULT_STRATEGY) != Z_OK) return n + n/2 + 1024;
	unsigned long b = deflateBound(&zs, n);
	deflateEnd(&zs);
	return b;
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

// deflateExact は zlib の raw deflate をレベル指定で実行する。
func deflateExact(plain []byte, level int) ([]byte, error) {
	bound := uint64(C.deflate_bound(C.ulong(len(plain))))
	out := make([]byte, bound)
	var inPtr *C.uchar
	if len(plain) > 0 {
		inPtr = (*C.uchar)(unsafe.Pointer(&plain[0]))
	}
	n := C.deflate_exact(inPtr, C.ulong(len(plain)),
		(*C.uchar)(unsafe.Pointer(&out[0])), C.ulong(bound), C.int(level))
	if n < 0 {
		return nil, fmt.Errorf("zlib deflate 失敗 (code %d)", int(n))
	}
	return out[:n], nil
}

// 探索順: zlib デフォルト6と最高9が実世界の大半を占めるので先に試す。
var levelOrder = []int{6, 9, 1, 2, 3, 4, 5, 7, 8}

// findLevel は deflate ストリームをビット一致再現できる zlib レベルを探す。
func findLevel(plain, deflateStream []byte) (int, bool) {
	for _, level := range levelOrder {
		candidate, err := deflateExact(plain, level)
		if err != nil {
			return 0, false
		}
		if bytes.Equal(candidate, deflateStream) {
			return level, true
		}
	}
	return 0, false
}

// TryUnwrap は gzip ストリームを「展開データ+レシピ」に分解する。
// ビット一致で再構成できる場合のみ結果を返す(それ以外は ok=false)。
func TryUnwrap(orig []byte) (*Unwrapped, bool) {
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
	plain, err := io.ReadAll(fr)
	fr.Close()
	if err != nil {
		return nil, false // マルチメンバー等もここで弾かれる
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
