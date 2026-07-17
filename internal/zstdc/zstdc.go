//go:build cgo

// Package zstdc はシステムの本家 libzstd をエンコーダとして使う薄いラッパー。
//
// 純Go実装(klauspost/compress)の最高レベルは本家の level 11 相当で、
// 本家の level 19 はテキスト系でさらに 8〜10% 小さくなる(RESEARCH.md §4.2)。
// さらにオフライン経路(リージョン圧縮・背景再圧縮)には level 22(ultra)+
// 入力サイズに合わせた大窓 + long-distance matching を使う(CompressMax)。
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

typedef struct ZSTD_CCtx_s ZSTD_CCtx;
ZSTD_CCtx* ZSTD_createCCtx(void);
size_t ZSTD_freeCCtx(ZSTD_CCtx* cctx);
size_t ZSTD_CCtx_setParameter(ZSTD_CCtx* cctx, int param, int value);
size_t ZSTD_compress2(ZSTD_CCtx* cctx, void* dst, size_t dstCapacity,
                      const void* src, size_t srcSize);
*/
import "C"

import (
	"fmt"
	"math/bits"
	"unsafe"
)

// ZSTD_cParameter の ABI 値(zstd.h の enum。安定 API)。
const (
	pCompressionLevel = 100 // ZSTD_c_compressionLevel
	pWindowLog        = 101 // ZSTD_c_windowLog
	pEnableLDM        = 160 // ZSTD_c_enableLongDistanceMatching
)

// Available はこのビルドで本家 libzstd が使えるかを返す。
func Available() bool { return true }

// Level は取り込み経路で使う圧縮レベル。19 は zstd CLI の -19 と同じで、
// 速度と圧縮率のバランスが実用最高域。
const Level = 19

// MaxLevel はオフライン経路(リージョン・背景再圧縮)で使う最高レベル。
// level 22(--ultra 相当)は 19 よりさらに数%小さいが数倍遅いため、
// ユーザーを待たせない背景処理だけで使う。
const MaxLevel = 22

// maxWindowLog は CompressMax の窓の上限(64MiB)。klauspost デコーダの
// 既定上限(512MiB)より十分小さく、復号側の設定変更なしで安全に読める。
const maxWindowLog = 26

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

// windowLogFor は入力全体への後方参照が届く窓ログ(上限 maxWindowLog)を返す。
// 入力が小さいときは 0(= レベル既定に任せる)。
func windowLogFor(n int) int {
	if n <= 8<<20 {
		return 0 // level 22 の既定窓(8MiB)で全体が見える
	}
	wl := bits.Len(uint(n - 1))
	if wl > maxWindowLog {
		wl = maxWindowLog
	}
	return wl
}

// CompressMax は data を level 22 + 入力サイズに合わせた大窓 + LDM で
// 圧縮する(オフライン経路用の最強設定)。出力は標準 zstd フレーム。
func CompressMax(data []byte) ([]byte, error) {
	cctx := C.ZSTD_createCCtx()
	if cctx == nil {
		return nil, fmt.Errorf("libzstd CCtx の作成に失敗")
	}
	defer C.ZSTD_freeCCtx(cctx)

	setParam := func(p, v int) error {
		if rc := C.ZSTD_CCtx_setParameter(cctx, C.int(p), C.int(v)); C.ZSTD_isError(rc) != 0 {
			return fmt.Errorf("libzstd パラメータ設定に失敗 (param %d)", p)
		}
		return nil
	}
	if err := setParam(pCompressionLevel, MaxLevel); err != nil {
		return nil, err
	}
	if wl := windowLogFor(len(data)); wl > 0 {
		if err := setParam(pWindowLog, wl); err != nil {
			return nil, err
		}
		// 大窓では long-distance matching が遠距離の反復を拾う
		if err := setParam(pEnableLDM, 1); err != nil {
			return nil, err
		}
	}

	bound := uint64(C.ZSTD_compressBound(C.size_t(len(data))))
	out := make([]byte, bound)
	var src unsafe.Pointer
	if len(data) > 0 {
		src = unsafe.Pointer(&data[0])
	}
	n := C.ZSTD_compress2(cctx, unsafe.Pointer(&out[0]), C.size_t(bound),
		src, C.size_t(len(data)))
	if C.ZSTD_isError(C.size_t(n)) != 0 {
		return nil, fmt.Errorf("libzstd 圧縮に失敗 (code %d)", uint64(n))
	}
	return out[:uint64(n)], nil
}
