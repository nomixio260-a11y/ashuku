package precomp

// HEVC トランスコードのコンテナ非依存エンジン(h264engine.go と同型)。
// Annex B / MP4(hvc1)のどちらも NAL 単位の capture / rebuild をここへ
// 集約する。v1 は I スライス(全イントラ・IRAP)のみ二次算術へ載せ替え、
// P/B スライスや対象外構文の NAL は原文のままスケルトンに置く。
// ただし一度走査を始めたスライスの失敗はストリーム全体の放棄(素通し)。

import "errors"

var errHEVCUnsupported = errors.New("hevc 対象外構文")
var errHEVCBadRecipe = errors.New("hevc レシピが不正です")

// hevcCapture は capture 側のエンジン状態。
type hevcCapture struct {
	spsMap map[int]*hevcSPS
	ppsMap map[int]*hevcPPS
	enc    *rangeEncoder
	model  *hevcSecModel
	pic    *hevcPicState
	picOK  bool
	coded  int
}

func newHEVCCapture() *hevcCapture {
	return &hevcCapture{
		spsMap: map[int]*hevcSPS{},
		ppsMap: map[int]*hevcPPS{},
		enc:    newRangeEncoder(),
		model:  newHEVCSecModel(),
	}
}

// observePS は SPS/PPS NAL(EBSP、NAL ヘッダ込み)を取り込む。
func (c *hevcCapture) observePS(nalData []byte) {
	typ := hevcNALType(nalData)
	if typ != hevcNALSPS && typ != hevcNALPPS {
		return
	}
	rbsp := unescapeRBSP(nalData)
	if len(rbsp) < 3 {
		return
	}
	if typ == hevcNALSPS {
		if s, ok := parseHEVCSPS(rbsp[2:]); ok {
			c.spsMap[s.spsID] = s
		}
	} else {
		if p, ok := parseHEVCPPS(rbsp[2:]); ok {
			c.ppsMap[p.ppsID] = p
		}
	}
}

// noteRawNAL は原文のまま置く NAL の副作用: スライスが素通しされたら
// ピクチャ状態の継続を無効化する(後続の非先頭スライスを誤継続させない)。
func (c *hevcCapture) noteRawNAL(nalData []byte) {
	if hevcIsSlice(hevcNALType(nalData)) {
		c.picOK = false
	}
}

// captureNAL はスライス NAL のトランスコードを試みる。
// (ヘッダブロブ, ヘッダビット数, true, nil) = 符号化済み。
// (nil, 0, false, nil) = 原文のままにすべき NAL。err != nil は全体放棄。
func (c *hevcCapture) captureNAL(nalData []byte) ([]byte, int, bool, error) {
	if len(nalData) < 3 {
		return nil, 0, false, nil
	}
	typ := hevcNALType(nalData)
	if typ == hevcNALSPS || typ == hevcNALPPS {
		c.observePS(nalData)
		return nil, 0, false, nil
	}
	if !hevcIsSlice(typ) {
		return nil, 0, false, nil
	}
	rbsp := unescapeRBSP(nalData)
	if len(rbsp) < 4 {
		return nil, 0, false, nil
	}
	body := rbsp[2:]
	ppsID, ok := hevcPeekSlicePPSID(body, typ)
	if !ok {
		return nil, 0, false, nil
	}
	pps := c.ppsMap[ppsID]
	if pps == nil {
		return nil, 0, false, nil
	}
	sps := c.spsMap[pps.spsID]
	if sps == nil || sps.chromaFormatIDC != 1 {
		return nil, 0, false, nil
	}
	r := &h264Reader{b: body}
	sl, ok := parseHEVCSliceHeader(r, sps, pps, typ)
	if !ok || sl.headerBits%8 != 0 {
		return nil, 0, false, nil // P/B・従属スライス等は原文のまま
	}
	if len(sl.entryOffsets) > 254 {
		return nil, 0, false, nil
	}
	if len(sl.entryOffsets) > 0 {
		conv, ok := hevcEntryOffsetsRBSP(nalData, 2+sl.headerBits/8, sl.entryOffsets)
		if !ok {
			return nil, 0, false, nil
		}
		sl.entryOffsets = conv
	}
	// ピクチャ状態: 先頭スライスで作り直し。途中スライスは先行スライスを
	// 走査済みの場合のみ継続できる。
	if sl.firstSlice {
		c.pic = newHEVCPicState(sps, pps)
		c.picOK = true
	} else if !c.picOK || c.pic == nil || c.pic.sps != sps {
		return nil, 0, false, nil
	}

	hb := 2 + sl.headerBits/8
	cr := &h264Reader{b: body, pos: sl.headerBits}
	w := &h264Writer{}
	sink := &hevcTeeSink{
		d: newCabacDecoder(cr), rc: c.enc, m: c.model,
		w: w, ce: newCabacEncoder(w),
		body: body, subStart: sl.headerBits / 8,
	}
	// ここからビンを二次算術へ書くため、失敗時はストリーム全体を放棄する
	if !hevcWalkSliceData(sink, sink, c.pic, sl) {
		return nil, 0, false, errHEVCUnsupported
	}
	if !sink.finishSub(len(body)) {
		return nil, 0, false, errHEVCUnsupported
	}
	// ヘッダブロブ: [rbsp[:hb]][サブ数 1B][delta 1B, tailLen 1B, tail]...
	blob := make([]byte, 0, hb+2+len(sink.fixes)*3)
	blob = append(blob, rbsp[:hb]...)
	blob = append(blob, byte(len(sink.fixes)))
	for _, f := range sink.fixes {
		blob = append(blob, byte(f.delta), byte(len(f.tail)))
		blob = append(blob, f.tail...)
	}
	c.coded++
	return blob, sl.headerBits, true, nil
}

// hevcRebuild は rebuild 側のエンジン状態。
type hevcRebuild struct {
	spsMap map[int]*hevcSPS
	ppsMap map[int]*hevcPPS
	dec    *rangeDecoder
	model  *hevcSecModel
	pic    *hevcPicState
	picOK  bool
}

func newHEVCRebuild(arith []byte) *hevcRebuild {
	return &hevcRebuild{
		spsMap: map[int]*hevcSPS{},
		ppsMap: map[int]*hevcPPS{},
		dec:    newRangeDecoder(arith),
		model:  newHEVCSecModel(),
	}
}

func (d *hevcRebuild) observePS(nalData []byte) {
	(&hevcCapture{spsMap: d.spsMap, ppsMap: d.ppsMap}).observePS(nalData)
}

// rebuildNAL はスライス NAL を再構成し EBSP(NAL ヘッダ込み)を返す。
func (d *hevcRebuild) rebuildNAL(blob []byte, hdrBits int) ([]byte, error) {
	hb := 2 + hdrBits/8
	if hdrBits%8 != 0 || len(blob) < hb+1 {
		return nil, errHEVCBadRecipe
	}
	typ := hevcNALType(blob)
	body := blob[2:hb]
	ppsID, ok := hevcPeekSlicePPSID(body, typ)
	if !ok {
		return nil, errHEVCBadRecipe
	}
	pps := d.ppsMap[ppsID]
	if pps == nil {
		return nil, errHEVCBadRecipe
	}
	sps := d.spsMap[pps.spsID]
	if sps == nil {
		return nil, errHEVCBadRecipe
	}
	r := &h264Reader{b: body}
	sl, ok := parseHEVCSliceHeader(r, sps, pps, typ)
	if !ok || sl.headerBits != hdrBits {
		return nil, errHEVCBadRecipe
	}
	// [delta][tail] レコードの取り出し
	p := hb
	nSub := int(blob[p])
	p++
	fixes := make([]hevcSubFix, 0, nSub)
	for i := 0; i < nSub; i++ {
		if p+2 > len(blob) {
			return nil, errHEVCBadRecipe
		}
		delta := int(blob[p])
		tl := int(blob[p+1])
		p += 2
		if p+tl > len(blob) {
			return nil, errHEVCBadRecipe
		}
		fixes = append(fixes, hevcSubFix{delta: delta, tail: blob[p : p+tl]})
		p += tl
	}
	if p != len(blob) || nSub != len(sl.entryOffsets)+1 {
		return nil, errHEVCBadRecipe
	}

	if sl.firstSlice {
		d.pic = newHEVCPicState(sps, pps)
		d.picOK = true
	} else if !d.picOK || d.pic == nil || d.pic.sps != sps {
		return nil, errHEVCBadRecipe
	}

	w := &h264Writer{}
	sink := &hevcRebuildSink{
		dec: d.dec, m: d.model,
		w: w, ce: newCabacEncoder(w),
		fixes: fixes,
	}
	if !hevcWalkSliceData(sink, sink, d.pic, sl) {
		return nil, errHEVCBadRecipe
	}
	if !sink.finishSub() {
		return nil, errHEVCBadRecipe
	}
	out := make([]byte, 0, hb+len(sink.out))
	out = append(out, blob[:hb]...)
	out = append(out, sink.out...)
	return escapeRBSP(out), nil
}
