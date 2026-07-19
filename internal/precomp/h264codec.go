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
//
// 旧形式(v1): Raw / Hdr にバイト列を直接持つ(JSON で 4/3 倍に膨れる)。
// 新形式(v2): バイト列は Chunked 先頭のブロブ(rawBlob+hdrBlob)に置き、
// ここには長さだけを持つ。Reconstruct は両形式を受け付ける。
type H264NALRecipe struct {
	Start  int    `json:"s,omitempty"`  // スライス: スタートコード長(3/4)
	Raw    []byte `json:"r,omitempty"`  // v1: 非スライス NAL の原文(EBSP)
	Hdr    []byte `json:"h,omitempty"`  // v1: スライスヘッダの RBSP バイト
	Bits   int    `json:"b,omitempty"`  // スライスヘッダのビット数(NALヘッダ除く)
	RawLen int    `json:"rl,omitempty"` // v2: 原文セグメント長(スタートコード込み、連結可)
	HdrLen int    `json:"hl,omitempty"` // v2: ヘッダブロブ内の長さ
}

// H264Recipe は H.264 再構成レシピ。
type H264Recipe struct {
	NALs []H264NALRecipe `json:"nals"`
	RawN int             `json:"rn,omitempty"` // v2: Chunked 先頭の原文ブロブ長
	HdrN int             `json:"hn,omitempty"` // v2: 続くヘッダブロブ長
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
	var rawBlob, hdrBlob []byte
	addRaw := func(b []byte) {
		if len(b) == 0 {
			return
		}
		// 直前も Raw なら結合(エントリ数を抑える)
		if n := len(recipe.NALs); n > 0 && recipe.NALs[n-1].RawLen > 0 && recipe.NALs[n-1].HdrLen == 0 {
			recipe.NALs[n-1].RawLen += len(b)
		} else {
			recipe.NALs = append(recipe.NALs, H264NALRecipe{RawLen: len(b)})
		}
		rawBlob = append(rawBlob, b...)
	}
	startCodeOf := func(n int) []byte {
		if n == 3 {
			return []byte{0, 0, 1}
		}
		return []byte{0, 0, 0, 1}
	}

	for _, nal := range nals {
		if len(nal.data) < 1 {
			return nil, false
		}
		hdrRBSP, hdrBits, coded, err := eng.captureNAL(nal.data)
		if err != nil {
			return nil, false // 対象外構文・不正 → 全体を素通し
		}
		if coded {
			recipe.NALs = append(recipe.NALs, H264NALRecipe{Start: nal.startLen, HdrLen: len(hdrRBSP), Bits: hdrBits})
			hdrBlob = append(hdrBlob, hdrRBSP...)
		} else {
			addRaw(startCodeOf(nal.startLen))
			addRaw(nal.data)
		}
	}
	if eng.coded == 0 {
		return nil, false
	}
	arith := eng.enc.finish()
	recipe.RawN = len(rawBlob)
	recipe.HdrN = len(hdrBlob)
	chunked := make([]byte, 0, len(rawBlob)+len(hdrBlob)+len(arith))
	chunked = append(chunked, rawBlob...)
	chunked = append(chunked, hdrBlob...)
	chunked = append(chunked, arith...)
	// 採用ゲート: ブロブ込み本文+レシピ概算が元より小さいこと
	if len(chunked)+len(recipe.NALs)*14+64 >= len(orig) {
		return nil, false
	}
	// 最終安全弁: 完全復元のバイト一致
	rt, err := ReconstructH264(recipe, chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &H264Unwrapped{Chunked: chunked, Recipe: recipe}, true
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

// ReconstructH264 はレシピと Chunked 内容から元の Annex B をビット単位で戻す。
// v1(バイト列を JSON に内包)と v2(Chunked 先頭ブロブ)の両形式に対応。
func ReconstructH264(recipe *H264Recipe, chunked []byte) ([]byte, error) {
	if recipe == nil || len(recipe.NALs) == 0 ||
		recipe.RawN < 0 || recipe.HdrN < 0 || recipe.RawN+recipe.HdrN > len(chunked) {
		return nil, errH264BadRecipe
	}
	rawBlob := chunked[:recipe.RawN]
	hdrBlob := chunked[recipe.RawN : recipe.RawN+recipe.HdrN]
	arith := chunked[recipe.RawN+recipe.HdrN:]
	eng := newH264Rebuild(arith)
	var out []byte
	rp, hp := 0, 0

	startCode := func(n int) []byte {
		if n == 3 {
			return []byte{0, 0, 1}
		}
		return []byte{0, 0, 0, 1}
	}
	// v2 の原文セグメントは SPS/PPS をスキャンして取り込む(スタートコード込み)
	feedPSFromRaw := func(b []byte) {
		segs, ok := splitAnnexB(b)
		if !ok {
			return
		}
		for _, s := range segs {
			eng.observePS(s.data)
		}
	}

	for _, nr := range recipe.NALs {
		switch {
		case nr.RawLen > 0 && nr.HdrLen == 0: // v2 原文セグメント
			if rp+nr.RawLen > len(rawBlob) {
				return nil, errH264BadRecipe
			}
			seg := rawBlob[rp : rp+nr.RawLen]
			rp += nr.RawLen
			out = append(out, seg...)
			feedPSFromRaw(seg)
		case nr.HdrLen > 0: // v2 スライス
			if hp+nr.HdrLen > len(hdrBlob) {
				return nil, errH264BadRecipe
			}
			hdr := hdrBlob[hp : hp+nr.HdrLen]
			hp += nr.HdrLen
			nal, err := eng.rebuildNAL(hdr, nr.Bits)
			if err != nil {
				return nil, err
			}
			out = append(out, startCode(nr.Start)...)
			out = append(out, nal...)
		case nr.Raw != nil: // v1 原文 NAL
			out = append(out, startCode(nr.Start)...)
			out = append(out, nr.Raw...)
			eng.observePS(nr.Raw)
		default: // v1 スライス
			nal, err := eng.rebuildNAL(nr.Hdr, nr.Bits)
			if err != nil {
				return nil, err
			}
			out = append(out, startCode(nr.Start)...)
			out = append(out, nal...)
		}
	}
	return out, nil
}
