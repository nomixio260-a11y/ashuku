//go:build cgo

// Package zstdc はシステムの本家 libzstd をエンコーダとして使う薄いラッパー。
//
// 純Go実装(klauspost/compress)の最高レベルは本家の level 11 相当で、
// 本家の level 19 はテキスト系でさらに 8〜10% 小さくなる(RESEARCH.md §4.2)。
// zstd フレームは自己記述的な標準形式なので、エンコーダだけ本家に差し替えても
// 復号は既存の純Goデコーダのままでよく、保存形式は一切変わらない。
//
// 開発ヘッダ(zstd.h)に依存しないよう、必要な関数だけを自前宣言して
// libzstd.so.1 に直接リンクする。CGO 無効ビルドではスタブに落ちる。
package zstdc

/*
#cgo LDFLAGS: -l:libzstd.so.1
#include <stddef.h>

// zstd.h を使わずに必要なAPIだけ宣言する(ABI は安定)
size_t ZSTD_compressBound(size_t srcSize);
size_t ZSTD_compress(void* dst, size_t dstCapacity,
                     const void* src, size_t srcSize, int compressionLevel);
unsigned ZSTD_isError(size_t code);
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Available はこのビルドで本家 libzstd が使えるかを返す。
func Available() bool { return true }

// Level は本家エンコーダで使う圧縮レベル。19 は zstd CLI の -19 と同じで、
// 実用最高域(--ultra 領域はメモリ・時間コストが急増するため使わない)。
const Level = 19

// Compress は data を本家 libzstd の level 19 で圧縮する。
// 出力は標準 zstd フレーム(純Goデコーダで伸長可能)。
func Compress(data []byte) ([]byte, error) {
	if len(data) == 0 {
		// 空入力はフレームだけ返す(呼び出し側は raw フォールバックで拾う)
		data = []byte{}
	}
	bound := uint64(C.ZSTD_compressBound(C.size_t(len(data))))
	out := make([]byte, bound)
	var src unsafe.Pointer
	if len(data) > 0 {
		src = unsafe.Pointer(&data[0])
	}
	n := C.ZSTD_compress(unsafe.Pointer(&out[0]), C.size_t(bound),
		src, C.size_t(len(data)), C.int(Level))
	if C.ZSTD_isError(C.size_t(n)) != 0 {
		return nil, fmt.Errorf("libzstd 圧縮に失敗 (code %d)", uint64(n))
	}
	return out[:uint64(n)], nil
}
