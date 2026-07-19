package precomp

// H.264 CAVLC ストリームの可逆再圧縮(トップレベル)。
//
// 対象: Annex B エレメンタリストリームの Constrained Baseline 系
// (CAVLC・プログレッシブ・I/P スライス・FMO なし)。監視カメラ・webcam・
// 会議録画など、低遅延ハードウェアエンコーダの定番領域で、ストレージを
// 大量に食う現役ニッチ。スマホ動画などの CABAC は既に算術符号なので対象外
// (情報理論的に可逆では縮まない。§4.23)——判定して安全に素通しする。
//
// 手法: スライスデータの CAVLC を構文要素へ解析し(ピクセル復号なし)、
// 文脈適応算術符号で再符号化する。SPS/PPS/SEI とスライスヘッダは原文の
// ままレシピに保存。採用前に**必ず全体を復元してバイト一致検証**し、
// 一致しない・対象外構文のストリームは素通しする(JPEG §4.21 と同じ安全弁)。

import (
	"bytes"
	"errors"
)

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

	spsMap := map[int]*h264SPS{}
	ppsMap := map[int]*h264PPS{}
	enc := newRangeEncoder()
	model := newH264Model()
	var nzMap *h264NZ
	recipe := &H264Recipe{}
	coded := 0
	skeleton := 0

	for _, nal := range nals {
		if len(nal.data) < 1 {
			return nil, false
		}
		hdr := nal.data[0]
		typ := int(hdr & 0x1F)
		refIDC := int(hdr >> 5)
		if typ == 7 || typ == 8 {
			rbsp := unescapeRBSP(nal.data)
			if typ == 7 {
				if s, ok2 := parseSPS(rbsp[1:]); ok2 {
					id := spsIDOf(rbsp[1:])
					spsMap[id] = s
				}
			} else {
				if p, ok2 := parsePPS(rbsp[1:]); ok2 {
					id := ppsIDOf(rbsp[1:])
					ppsMap[id] = p
				}
			}
		}
		if typ != 1 && typ != 5 {
			recipe.NALs = append(recipe.NALs, H264NALRecipe{Start: nal.startLen, Raw: nal.data})
			skeleton += len(nal.data)
			continue
		}
		// スライス
		rbsp := unescapeRBSP(nal.data)
		payload := rbsp[1:]
		pre := &h264Reader{b: payload}
		if _, err := pre.ue(); err != nil { // first_mb
			return nil, false
		}
		if _, err := pre.ue(); err != nil { // slice_type
			return nil, false
		}
		ppsID, err := pre.ue()
		if err != nil {
			return nil, false
		}
		pps := ppsMap[int(ppsID)]
		if pps == nil {
			return nil, false
		}
		sps := spsMap[pps.spsID]
		if sps == nil || pps.entropyCodingMode {
			return nil, false
		}
		r := &h264Reader{b: payload}
		sl, ok2 := parseSliceHeader(r, sps, pps, typ, refIDC)
		if !ok2 {
			return nil, false
		}
		if nzMap == nil || nzMap.mbW != sps.picWidthInMbs || nzMap.mbH != sps.picHeightInMbs {
			nzMap = newH264NZ(sps.picWidthInMbs, sps.picHeightInMbs)
		}
		t := &h264T{capture: true, br: r, enc: enc, m: model}
		if err := t.transcodeSliceData(sps, sl, nzMap); err != nil {
			return nil, false
		}
		hdrBytes := 1 + (sl.headerBits+7)/8
		recipe.NALs = append(recipe.NALs, H264NALRecipe{
			Start: nal.startLen,
			Hdr:   append([]byte(nil), rbsp[:hdrBytes]...),
			Bits:  sl.headerBits,
		})
		skeleton += hdrBytes
		coded++
	}
	if coded == 0 {
		return nil, false
	}
	arith := enc.finish()
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
		return nil, errors.New("h264 レシピが不正です")
	}
	dec := newRangeDecoder(chunked)
	model := newH264Model()
	spsMap := map[int]*h264SPS{}
	ppsMap := map[int]*h264PPS{}
	var nzMap *h264NZ
	var out []byte

	startCode := func(n int) []byte {
		if n == 3 {
			return []byte{0, 0, 1}
		}
		return []byte{0, 0, 0, 1}
	}

	for _, nr := range recipe.NALs {
		if nr.Raw != nil {
			out = append(out, startCode(nr.Start)...)
			out = append(out, nr.Raw...)
			typ := int(nr.Raw[0] & 0x1F)
			if typ == 7 || typ == 8 {
				rbsp := unescapeRBSP(nr.Raw)
				if typ == 7 {
					if s, ok := parseSPS(rbsp[1:]); ok {
						spsMap[spsIDOf(rbsp[1:])] = s
					}
				} else {
					if p, ok := parsePPS(rbsp[1:]); ok {
						ppsMap[ppsIDOf(rbsp[1:])] = p
					}
				}
			}
			continue
		}
		// スライス NAL の再構成
		if len(nr.Hdr) < 2 {
			return nil, errors.New("h264 スライスヘッダが不正です")
		}
		nalHdr := nr.Hdr[0]
		typ := int(nalHdr & 0x1F)
		refIDC := int(nalHdr >> 5)
		payload := nr.Hdr[1:]
		pre := &h264Reader{b: payload}
		if _, err := pre.ue(); err != nil {
			return nil, err
		}
		if _, err := pre.ue(); err != nil {
			return nil, err
		}
		ppsID, err := pre.ue()
		if err != nil {
			return nil, err
		}
		pps := ppsMap[int(ppsID)]
		if pps == nil {
			return nil, errors.New("h264: PPS が見つかりません")
		}
		sps := spsMap[pps.spsID]
		if sps == nil {
			return nil, errors.New("h264: SPS が見つかりません")
		}
		hr := &h264Reader{b: payload}
		sl, ok := parseSliceHeader(hr, sps, pps, typ, refIDC)
		if !ok || sl.headerBits != nr.Bits {
			return nil, errors.New("h264: スライスヘッダの再解析に失敗")
		}
		if nzMap == nil || nzMap.mbW != sps.picWidthInMbs || nzMap.mbH != sps.picHeightInMbs {
			nzMap = newH264NZ(sps.picWidthInMbs, sps.picHeightInMbs)
		}
		bw := &h264Writer{}
		// NAL ヘッダ + スライスヘッダを逐語コピー
		cr := &h264Reader{b: nr.Hdr}
		if err := copyBits(bw, cr, 8+nr.Bits); err != nil {
			return nil, err
		}
		t := &h264T{capture: false, bw: bw, dec: dec, m: model}
		if err := t.transcodeSliceData(sps, sl, nzMap); err != nil {
			return nil, err
		}
		// rbsp_slice_trailing_bits
		bw.u1(1)
		for bw.nbit%8 != 0 {
			bw.u1(0)
		}
		out = append(out, startCode(nr.Start)...)
		out = append(out, escapeRBSP(bw.b)...)
	}
	return out, nil
}
