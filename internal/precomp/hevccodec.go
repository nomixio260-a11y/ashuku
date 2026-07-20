package precomp

// HEVC(H.265)Annex B ストリームの可逆再圧縮(コンテナ層)。
//
// 対象: 全イントラ/IRAP の I スライス CABAC(4:2:0、タイルなし。WPP は
// エントリポイント+文脈スナップショットで追随)。HEIC 由来の静止画や
// all-intra 収録が主対象で、混在 GOP でも I スライスだけ載せ替え、
// P/B スライスは原文のままスケルトンへ置く。採用前に必ず全体を復元して
// バイト一致検証し、失敗時は素通しする。

import (
	"bytes"
	"encoding/json"
)

// HEVCNALRecipe は 1 セグメントの再構成情報(H264NALRecipe v2 と同型)。
type HEVCNALRecipe struct {
	Start  int `json:"s,omitempty"`  // スライス: スタートコード長(3/4)
	Bits   int `json:"b,omitempty"`  // スライスヘッダのビット数(NAL ヘッダ除く)
	RawLen int `json:"rl,omitempty"` // 原文セグメント長(スタートコード込み)
	HdrLen int `json:"hl,omitempty"` // ヘッダブロブ内の長さ
}

// HEVCRecipe は HEVC 再構成レシピ。
type HEVCRecipe struct {
	NALs []HEVCNALRecipe `json:"nals"`
	RawN int             `json:"rn,omitempty"`
	HdrN int             `json:"hn,omitempty"`
}

// HEVCUnwrapped は分解結果。
type HEVCUnwrapped struct {
	Chunked []byte
	Recipe  *HEVCRecipe
}

// TryUnwrapHEVC は HEVC の I スライスを文脈算術に再符号化する。
func TryUnwrapHEVC(orig []byte, maxPlain int64) (*HEVCUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 64 || !IsHEVC(orig) {
		return nil, false
	}
	nals, ok := splitAnnexB(orig)
	if !ok {
		return nil, false
	}

	eng := newHEVCCapture()
	recipe := &HEVCRecipe{}
	var rawBlob, hdrBlob []byte
	addRaw := func(b []byte) {
		if len(b) == 0 {
			return
		}
		if n := len(recipe.NALs); n > 0 && recipe.NALs[n-1].RawLen > 0 && recipe.NALs[n-1].HdrLen == 0 {
			recipe.NALs[n-1].RawLen += len(b)
		} else {
			recipe.NALs = append(recipe.NALs, HEVCNALRecipe{RawLen: len(b)})
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
		if len(nal.data) < 2 {
			return nil, false
		}
		blob, hdrBits, coded, err := eng.captureNAL(nal.data)
		if err != nil {
			return nil, false // 走査途中の失敗 → 全体を素通し
		}
		if coded {
			recipe.NALs = append(recipe.NALs, HEVCNALRecipe{Start: nal.startLen, HdrLen: len(blob), Bits: hdrBits})
			hdrBlob = append(hdrBlob, blob...)
		} else {
			eng.noteRawNAL(nal.data)
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
	// 採用ゲート: 保存形は zstd 越しなので、zstd 概算で元と比較する
	// (SEI テキスト等の生セグメントは両側で同様に縮むため、実質は
	// 算術ストリーム対 CABAC の比較になる)。レシピは実 JSON 長で見積もる。
	rj, err := json.Marshal(recipe)
	if err != nil {
		return nil, false
	}
	zo := jpegProbeEncoder.EncodeAll(orig, nil)
	zc := jpegProbeEncoder.EncodeAll(chunked, nil)
	if len(zc)+len(rj)+48 >= len(zo) {
		return nil, false
	}
	rt, err := ReconstructHEVC(recipe, chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &HEVCUnwrapped{Chunked: chunked, Recipe: recipe}, true
}

// ReconstructHEVC はレシピと Chunked 内容から元の Annex B を戻す。
func ReconstructHEVC(recipe *HEVCRecipe, chunked []byte) ([]byte, error) {
	if recipe == nil || len(recipe.NALs) == 0 ||
		recipe.RawN < 0 || recipe.HdrN < 0 || recipe.RawN+recipe.HdrN > len(chunked) {
		return nil, errHEVCBadRecipe
	}
	rawBlob := chunked[:recipe.RawN]
	hdrBlob := chunked[recipe.RawN : recipe.RawN+recipe.HdrN]
	arith := chunked[recipe.RawN+recipe.HdrN:]
	eng := newHEVCRebuild(arith)
	var out []byte
	rp, hp := 0, 0

	startCode := func(n int) []byte {
		if n == 3 {
			return []byte{0, 0, 1}
		}
		return []byte{0, 0, 0, 1}
	}
	feedPSFromRaw := func(b []byte) {
		segs, ok := splitAnnexB(b)
		if !ok {
			return
		}
		for _, s := range segs {
			eng.observePS(s.data)
			if hevcIsSlice(hevcNALType(s.data)) {
				eng.picOK = false
			}
		}
	}

	for _, nr := range recipe.NALs {
		switch {
		case nr.RawLen > 0 && nr.HdrLen == 0:
			if rp+nr.RawLen > len(rawBlob) {
				return nil, errHEVCBadRecipe
			}
			seg := rawBlob[rp : rp+nr.RawLen]
			rp += nr.RawLen
			out = append(out, seg...)
			feedPSFromRaw(seg)
		case nr.HdrLen > 0:
			if hp+nr.HdrLen > len(hdrBlob) {
				return nil, errHEVCBadRecipe
			}
			blob := hdrBlob[hp : hp+nr.HdrLen]
			hp += nr.HdrLen
			nal, err := eng.rebuildNAL(blob, nr.Bits)
			if err != nil {
				return nil, err
			}
			out = append(out, startCode(nr.Start)...)
			out = append(out, nal...)
		default:
			return nil, errHEVCBadRecipe
		}
	}
	return out, nil
}
