package precomp

// H.264 CAVLC ストリームの可逆再圧縮(Annex B コンテナ層)。
//
// 対象: Annex B エレメンタリストリームの Constrained Baseline 系
// (CAVLC・プログレッシブ・I/P スライス・FMO なし)。監視カメラ・webcam・
// 会議録画など、低遅延ハードウェアエンコーダの定番領域で、ストレージを
// 大量に食う現役ニッチ。スマホ動画などの CABAC は既に算術符号なので対象外
// (情報理論的に可逆では縮まない。§4.23)——判定して安全に素通しする。
// MP4 コンテナ入りの CAVLC は mp4.go が同じエンジンで扱う。
//
// 手法: スライスデータの CAVLC を構文要素へ解析し(ピクセル復号なし)、
// 文脈適応算術符号で再符号化する。SPS/PPS/SEI とスライスヘッダは原文の
// ままレシピに保存。採用前に**必ず全体を復元してバイト一致検証**し、
// 一致しない・対象外構文のストリームは素通しする(JPEG §4.21 と同じ安全弁)。

import (
	"bytes"
	"errors"
)

var errH264BadRecipe = errors.New("h264 レシピが不正です")

// IsH264 は Annex B の H.264 エレメンタリストリームらしいかを判定する。
func IsH264(head []byte) bool {
	sl := startCodeLen(head)
	if sl == 0 || len(head) < sl+1 {
		return false
	}
	b := head[sl]
	if b>>7 != 0 { // forbidden_zero_bit
		return false
	}
	switch b & 0x1F {
	case 1, 5, 6, 7, 8, 9:
		return true
	}
	return false
}

// H264NALRecipe は1 NAL の再構成情報。
type H264NALRecipe struct {
	Start int    `json:"s"`           // スタートコード長(3/4)
	Raw   []byte `json:"r,omitempty"` // 非スライス NAL の原文(EBSP)
	Hdr   []byte `json:"h,omitempty"` // スライス: NALヘッダ+スライスヘッダの RBSP バイト
	Bits  int    `json:"b,omitempty"` // スライスヘッダのビット数(NALヘッダ除く)
}

// H264Recipe は H.264 再構成レシピ。
type H264Recipe struct {
	NALs []H264NALRecipe `json:"nals"`
}

// H264Unwrapped は分解結果。
type H264Unwrapped struct {
	Chunked []byte // 文脈算術ストリーム
	Recipe  *H264Recipe
}

// TryUnwrapH264 は CAVLC スライスを文脈算術に再符号化する。
func TryUnwrapH264(orig []byte, maxPlain int64) (*H264Unwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 32 || !IsH264(orig) {
		return nil, false
	}
	nals, ok := splitAnnexB(orig)
	if !ok {
		return nil, false
	}

	eng := newH264Capture()
	recipe := &H264Recipe{}
	skeleton := 0

	for _, nal := range nals {
		if len(nal.data) < 1 {
			return nil, false
		}
		hdrRBSP, hdrBits, coded, err := eng.captureNAL(nal.data)
		if err != nil {
			return nil, false // 対象外構文・不正 → 全体を素通し
		}
		if coded {
			recipe.NALs = append(recipe.NALs, H264NALRecipe{Start: nal.startLen, Hdr: hdrRBSP, Bits: hdrBits})
			skeleton += len(hdrRBSP)
		} else {
			recipe.NALs = append(recipe.NALs, H264NALRecipe{Start: nal.startLen, Raw: nal.data})
			skeleton += len(nal.data)
		}
	}
	if eng.coded == 0 {
		return nil, false
	}
	arith := eng.enc.finish()
	// 採用ゲート: 算術ストリーム+レシピ(JSON化でおよそ 4/3 倍)が元より小さいこと
	if len(arith)+skeleton*3/2+64 >= len(orig) {
		return nil, false
	}
	// 最終安全弁: 完全復元のバイト一致
	rt, err := ReconstructH264(recipe, arith)
	if err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &H264Unwrapped{Chunked: arith, Recipe: recipe}, true
}

// spsIDOf / ppsIDOf は RBSP 先頭から ID を読む(SPS はプロファイル等の後)。
func spsIDOf(rbsp []byte) int {
	r := &h264Reader{b: rbsp}
	if _, err := r.u(24); err != nil {
		return 0
	}
	v, err := r.ue()
	if err != nil {
		return 0
	}
	return int(v)
}

func ppsIDOf(rbsp []byte) int {
	r := &h264Reader{b: rbsp}
	v, err := r.ue()
	if err != nil {
		return 0
	}
	return int(v)
}

// ReconstructH264 はレシピと算術ストリームから元の Annex B をビット単位で戻す。
func ReconstructH264(recipe *H264Recipe, chunked []byte) ([]byte, error) {
	if recipe == nil || len(recipe.NALs) == 0 {
		return nil, errH264BadRecipe
	}
	eng := newH264Rebuild(chunked)
	var out []byte

	startCode := func(n int) []byte {
		if n == 3 {
			return []byte{0, 0, 1}
		}
		return []byte{0, 0, 0, 1}
	}

	for _, nr := range recipe.NALs {
		out = append(out, startCode(nr.Start)...)
		if nr.Raw != nil {
			out = append(out, nr.Raw...)
			eng.observePS(nr.Raw)
			continue
		}
		nal, err := eng.rebuildNAL(nr.Hdr, nr.Bits)
		if err != nil {
			return nil, err
		}
		out = append(out, nal...)
	}
	return out, nil
}
