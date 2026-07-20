package precomp

// MP4/MOV(hvc1/hev1)入り HEVC の可逆再圧縮。スマホ・アクションカメラの
// HEVC 動画と HEIF 系の動画バリアントが主対象。サンプル(長さ前置 NAL 列)
// を歩き、I スライスを hevcengine の capture へ、その他は原文のまま
// スケルトンへ置く。パラメータセットは hvcC ボックスから取り込む。

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"sort"
)

// MP4HEVCRecipe は MP4(HEVC)の再構成レシピ(MP4H264Recipe と同型)。
type MP4HEVCRecipe struct {
	Segs []MP4Seg `json:"segs"`
	RawN int      `json:"rn,omitempty"`
	HdrN int      `json:"hn,omitempty"`
}

// MP4HEVCUnwrapped は分解結果。
type MP4HEVCUnwrapped struct {
	Chunked []byte
	Recipe  *MP4HEVCRecipe
}

// TryUnwrapMP4HEVC は MP4 内の HEVC I スライスを再符号化する。
func TryUnwrapMP4HEVC(orig []byte, maxPlain int64) (*MP4HEVCUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 64 || !IsMP4(orig) {
		return nil, false
	}
	tr, ok := parseMP4VideoTrack(orig)
	if !ok || tr.codec != "hevc" || (len(tr.offsets) == 0 && !tr.fragmented) {
		return nil, false
	}
	type sample struct {
		off  int64
		size int
	}
	var samples []sample
	if tr.fragmented && len(tr.offsets) == 0 {
		frs, ok := mp4FragmentSamples(orig, tr.trackID)
		if !ok {
			return nil, false
		}
		samples = make([]sample, len(frs))
		for i, fs := range frs {
			samples[i] = sample{fs[0], int(fs[1])}
		}
	} else {
		samples = make([]sample, len(tr.offsets))
		for i := range tr.offsets {
			samples[i] = sample{tr.offsets[i], tr.sizes[i]}
		}
	}
	sort.Slice(samples, func(a, b int) bool { return samples[a].off < samples[b].off })
	var last int64
	for _, s := range samples {
		if s.off < last || s.size < 0 || s.off+int64(s.size) > int64(len(orig)) {
			return nil, false
		}
		last = s.off + int64(s.size)
	}

	eng := newHEVCCapture()
	for _, ps := range tr.psNALs {
		eng.observePS(ps)
	}

	recipe := &MP4HEVCRecipe{}
	var rawBlob, hdrBlob []byte
	pos := int64(0)
	addRaw := func(b []byte) {
		if len(b) == 0 {
			return
		}
		// 原文バイトを挟んだらピクチャ継続は無効(rebuild 側と同一規則)
		eng.picOK = false
		if n := len(recipe.Segs); n > 0 && recipe.Segs[n-1].RawLen > 0 && recipe.Segs[n-1].HdrLen == 0 {
			recipe.Segs[n-1].RawLen += len(b)
		} else {
			recipe.Segs = append(recipe.Segs, MP4Seg{RawLen: len(b)})
		}
		rawBlob = append(rawBlob, b...)
	}
	for _, s := range samples {
		addRaw(orig[pos:s.off])
		p := s.off
		end := s.off + int64(s.size)
		for p < end {
			if p+int64(tr.lenSize) > end {
				return nil, false
			}
			var l int64
			for i := 0; i < tr.lenSize; i++ {
				l = l<<8 | int64(orig[p+int64(i)])
			}
			p += int64(tr.lenSize)
			if l <= 0 || p+l > end {
				return nil, false
			}
			nal := orig[p : p+l]
			blob, hdrBits, coded, err := eng.captureNAL(nal)
			if err != nil {
				return nil, false
			}
			if coded {
				recipe.Segs = append(recipe.Segs, MP4Seg{HdrLen: len(blob), Bits: hdrBits, LenSize: tr.lenSize})
				hdrBlob = append(hdrBlob, blob...)
			} else {
				addRaw(orig[p-int64(tr.lenSize) : p+l])
			}
			p += l
		}
		pos = end
	}
	addRaw(orig[pos:])

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
	rj, err := json.Marshal(recipe)
	if err != nil {
		return nil, false
	}
	zo := jpegProbeEncoder.EncodeAll(orig, nil)
	zc := jpegProbeEncoder.EncodeAll(chunked, nil)
	if len(zc)+len(rj)+48 >= len(zo) {
		return nil, false
	}
	rt, err := ReconstructMP4HEVC(recipe, chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &MP4HEVCUnwrapped{Chunked: chunked, Recipe: recipe}, true
}

// ReconstructMP4HEVC はレシピとチャンク化内容から元の MP4 を戻す。
func ReconstructMP4HEVC(recipe *MP4HEVCRecipe, chunked []byte) ([]byte, error) {
	if recipe == nil || len(recipe.Segs) == 0 ||
		recipe.RawN < 0 || recipe.HdrN < 0 || recipe.RawN+recipe.HdrN > len(chunked) {
		return nil, errHEVCBadRecipe
	}
	rawBlob := chunked[:recipe.RawN]
	hdrBlob := chunked[recipe.RawN : recipe.RawN+recipe.HdrN]
	arith := chunked[recipe.RawN+recipe.HdrN:]
	eng := newHEVCRebuild(arith)
	feedHvcC(rawBlob, eng)

	var out []byte
	rp, hp := 0, 0
	for _, sg := range recipe.Segs {
		if sg.RawLen > 0 && sg.HdrLen == 0 {
			if rp+sg.RawLen > len(rawBlob) {
				return nil, errHEVCBadRecipe
			}
			seg := rawBlob[rp : rp+sg.RawLen]
			rp += sg.RawLen
			out = append(out, seg...)
			eng.picOK = false // capture 側 addRaw と同一規則
			continue
		}
		if hp+sg.HdrLen > len(hdrBlob) || sg.LenSize < 1 || sg.LenSize > 4 {
			return nil, errHEVCBadRecipe
		}
		blob := hdrBlob[hp : hp+sg.HdrLen]
		hp += sg.HdrLen
		nal, err := eng.rebuildNAL(blob, sg.Bits)
		if err != nil {
			return nil, err
		}
		for i := sg.LenSize - 1; i >= 0; i-- {
			out = append(out, byte(len(nal)>>uint(8*i)))
		}
		out = append(out, nal...)
	}
	return out, nil
}

// feedHvcC はバイト列から hvcC ボックスを探して PS を取り込む。
func feedHvcC(b []byte, eng *hevcRebuild) {
	for i := 0; i+8 < len(b); i++ {
		if b[i+4] != 'h' || b[i+5] != 'v' || b[i+6] != 'c' || b[i+7] != 'C' {
			continue
		}
		size := int(binary.BigEndian.Uint32(b[i:]))
		if size < 31 || i+size > len(b) {
			continue
		}
		ps, _, ok := parseHvcC(b[i+8 : i+size])
		if !ok {
			continue
		}
		for _, p := range ps {
			eng.observePS(p)
		}
	}
}
