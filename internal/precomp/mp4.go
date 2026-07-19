package precomp

// MP4(ISO BMFF)コンテナ入り H.264 CAVLC の可逆再圧縮。
//
// ドラレコ・DVR・旧型スマホ・画面録画などの MP4 は Baseline/CAVLC が多く、
// Annex B(h264codec.go)と中身は同じ NAL 列(長さ前置)なので、moov の
// サンプル表(stsd/stsc/stsz/stco)から各ビデオサンプルのファイル位置を求め、
// サンプル内の長さ前置 NAL を同じエンジンでトランスコードする。コンテナの
// 他のバイト(moov・音声トラック・その他)はスケルトンとして原文保存。
// NAL の再構成はビット単位に一致するので、ファイル全体もバイト一致で戻る
// (採用前に全体復元検証+読み出し SHA-256)。moof(fragmented MP4)や
// CABAC は対象外として素通し。

import (
	"bytes"
	"encoding/binary"
	"sort"
)

// IsMP4 は ISO BMFF(ftyp)らしいかを判定する。
func IsMP4(head []byte) bool {
	return len(head) >= 12 && bytes.Equal(head[4:8], []byte("ftyp"))
}

// MP4Seg はファイルの1区間: 原文バイト列 or トランスコード済み NAL。
// バイト列本体は Chunked 側(rawBlob/hdrBlob)に置き、レシピは長さだけを
// 持つ(音声トラックや moov をレシピ JSON に抱えると肥大するため。
// Chunked に置けば store の圧縮・重複排除も効く)。
type MP4Seg struct {
	RawLen  int `json:"r,omitempty"` // 原文セグメントの長さ
	HdrLen  int `json:"h,omitempty"` // スライスヘッダ RBSP の長さ
	Bits    int `json:"b,omitempty"` // スライスヘッダのビット数
	LenSize int `json:"l,omitempty"` // 長さ前置のバイト数(1/2/4)
}

// MP4H264Recipe は MP4 再構成レシピ。
// Chunked = 原文セグメント連結(RawN) + ヘッダ連結(HdrN) + 算術ストリーム。
type MP4H264Recipe struct {
	Segs []MP4Seg `json:"segs"`
	RawN int      `json:"rn"`
	HdrN int      `json:"hn"`
}

// MP4H264Unwrapped は分解結果。
type MP4H264Unwrapped struct {
	Chunked []byte
	Recipe  *MP4H264Recipe
}

// mp4Box は直下ボックスのイテレータ。
func mp4Boxes(b []byte, fn func(typ string, body []byte) bool) bool {
	i := 0
	for i+8 <= len(b) {
		size := int(binary.BigEndian.Uint32(b[i:]))
		typ := string(b[i+4 : i+8])
		hdr := 8
		if size == 1 { // 64bit サイズ
			if i+16 > len(b) {
				return false
			}
			s64 := binary.BigEndian.Uint64(b[i+8:])
			if s64 > uint64(len(b)-i) {
				return false
			}
			size = int(s64)
			hdr = 16
		} else if size == 0 { // ファイル末尾まで
			size = len(b) - i
		}
		if size < hdr || i+size > len(b) {
			return false
		}
		if !fn(typ, b[i+hdr:i+size]) {
			return false
		}
		i += size
	}
	return i == len(b)
}

func findBox(b []byte, typ string) []byte {
	var out []byte
	mp4Boxes(b, func(t string, body []byte) bool {
		if t == typ && out == nil {
			out = body
		}
		return true
	})
	return out
}

// mp4Track は1ビデオトラックのサンプル表。
type mp4Track struct {
	lenSize int
	spsList [][]byte
	ppsList [][]byte
	sizes   []int
	offsets []int64
}

// parseMP4VideoTrack は avc1 トラックのサンプル表を取り出す(1本のみ対応)。
func parseMP4VideoTrack(file []byte) (*mp4Track, bool) {
	moov := findBox(file, "moov")
	if moov == nil {
		return nil, false
	}
	if findBox(moov, "mvex") != nil { // fragmented MP4 は対象外
		return nil, false
	}
	var tr *mp4Track
	okAll := true
	mp4Boxes(moov, func(t string, body []byte) bool {
		if t != "trak" {
			return true
		}
		mdia := findBox(body, "mdia")
		if mdia == nil {
			return true
		}
		minf := findBox(mdia, "minf")
		if minf == nil {
			return true
		}
		stbl := findBox(minf, "stbl")
		if stbl == nil {
			return true
		}
		stsd := findBox(stbl, "stsd")
		if stsd == nil || len(stsd) < 8 {
			return true
		}
		// stsd: ver/flags(4) + entry_count(4) + entries
		n := int(binary.BigEndian.Uint32(stsd[4:8]))
		if n != 1 {
			return true
		}
		entry := stsd[8:]
		if len(entry) < 8 {
			return true
		}
		etyp := string(entry[4:8])
		if etyp != "avc1" && etyp != "avc3" {
			return true // 他コーデックのトラックは無視(スケルトンに残る)
		}
		if tr != nil { // 複数ビデオトラックは対象外
			okAll = false
			return false
		}
		// avc1: サイズ(4)+typ(4)+予約等 78 バイトの後に子ボックス(avcC)
		if len(entry) < 8+78 {
			return true
		}
		var avcC []byte
		mp4Boxes(entry[8+78:], func(ct string, cb []byte) bool {
			if ct == "avcC" && avcC == nil {
				avcC = cb
			}
			return true
		})
		if avcC == nil || len(avcC) < 7 {
			return true
		}
		t2 := &mp4Track{lenSize: int(avcC[4]&3) + 1}
		// SPS/PPS リスト
		p := 6
		nSPS := int(avcC[5] & 0x1F)
		for i := 0; i < nSPS; i++ {
			if p+2 > len(avcC) {
				return true
			}
			l := int(binary.BigEndian.Uint16(avcC[p:]))
			p += 2
			if p+l > len(avcC) {
				return true
			}
			t2.spsList = append(t2.spsList, avcC[p:p+l])
			p += l
		}
		if p >= len(avcC) {
			return true
		}
		nPPS := int(avcC[p])
		p++
		for i := 0; i < nPPS; i++ {
			if p+2 > len(avcC) {
				return true
			}
			l := int(binary.BigEndian.Uint16(avcC[p:]))
			p += 2
			if p+l > len(avcC) {
				return true
			}
			t2.ppsList = append(t2.ppsList, avcC[p:p+l])
			p += l
		}
		// stsz
		stsz := findBox(stbl, "stsz")
		if stsz == nil || len(stsz) < 12 {
			return true
		}
		uniform := int(binary.BigEndian.Uint32(stsz[4:8]))
		cnt := int(binary.BigEndian.Uint32(stsz[8:12]))
		if cnt <= 0 || cnt > 1<<22 {
			return true
		}
		t2.sizes = make([]int, cnt)
		if uniform != 0 {
			for i := range t2.sizes {
				t2.sizes[i] = uniform
			}
		} else {
			if len(stsz) < 12+4*cnt {
				return true
			}
			for i := 0; i < cnt; i++ {
				t2.sizes[i] = int(binary.BigEndian.Uint32(stsz[12+4*i:]))
			}
		}
		// stco / co64(チャンク先頭オフセット)
		var chunkOff []int64
		if stco := findBox(stbl, "stco"); stco != nil && len(stco) >= 8 {
			n := int(binary.BigEndian.Uint32(stco[4:8]))
			if len(stco) < 8+4*n {
				return true
			}
			for i := 0; i < n; i++ {
				chunkOff = append(chunkOff, int64(binary.BigEndian.Uint32(stco[8+4*i:])))
			}
		} else if co64 := findBox(stbl, "co64"); co64 != nil && len(co64) >= 8 {
			n := int(binary.BigEndian.Uint32(co64[4:8]))
			if len(co64) < 8+8*n {
				return true
			}
			for i := 0; i < n; i++ {
				chunkOff = append(chunkOff, int64(binary.BigEndian.Uint64(co64[8+8*i:])))
			}
		} else {
			return true
		}
		// stsc(チャンクごとのサンプル数)→ サンプルごとのオフセット
		stsc := findBox(stbl, "stsc")
		if stsc == nil || len(stsc) < 8 {
			return true
		}
		ne := int(binary.BigEndian.Uint32(stsc[4:8]))
		if len(stsc) < 8+12*ne || ne <= 0 {
			return true
		}
		type stscEnt struct{ first, per int }
		ents := make([]stscEnt, ne)
		for i := 0; i < ne; i++ {
			ents[i] = stscEnt{
				first: int(binary.BigEndian.Uint32(stsc[8+12*i:])),
				per:   int(binary.BigEndian.Uint32(stsc[8+12*i+4:])),
			}
		}
		t2.offsets = make([]int64, 0, cnt)
		si := 0
		for ci := 0; ci < len(chunkOff) && si < cnt; ci++ {
			per := 0
			for _, e := range ents {
				if e.first <= ci+1 {
					per = e.per
				}
			}
			off := chunkOff[ci]
			for k := 0; k < per && si < cnt; k++ {
				t2.offsets = append(t2.offsets, off)
				off += int64(t2.sizes[si])
				si++
			}
		}
		if si != cnt {
			return true
		}
		tr = t2
		return true
	})
	if !okAll || tr == nil {
		return nil, false
	}
	return tr, true
}

// TryUnwrapMP4H264 は MP4 内の H.264 CAVLC サンプルを再符号化する。
func TryUnwrapMP4H264(orig []byte, maxPlain int64) (*MP4H264Unwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 64 || !IsMP4(orig) {
		return nil, false
	}
	tr, ok := parseMP4VideoTrack(orig)
	if !ok || len(tr.offsets) == 0 {
		return nil, false
	}
	// サンプルをファイルオフセット順に(重複・範囲外は対象外)
	type sample struct {
		off  int64
		size int
	}
	samples := make([]sample, len(tr.offsets))
	for i := range tr.offsets {
		samples[i] = sample{tr.offsets[i], tr.sizes[i]}
	}
	sort.Slice(samples, func(a, b int) bool { return samples[a].off < samples[b].off })
	var last int64
	for _, s := range samples {
		if s.off < last || s.size < 0 || s.off+int64(s.size) > int64(len(orig)) {
			return nil, false
		}
		last = s.off + int64(s.size)
	}

	eng := newH264Capture()
	for _, sps := range tr.spsList {
		eng.observePS(sps)
	}
	for _, pps := range tr.ppsList {
		eng.observePS(pps)
	}

	recipe := &MP4H264Recipe{}
	var rawBlob, hdrBlob []byte
	pos := int64(0)
	addRaw := func(b []byte) {
		if len(b) == 0 {
			return
		}
		// 直前も Raw なら結合(セグメント数を抑える)
		if n := len(recipe.Segs); n > 0 && recipe.Segs[n-1].RawLen > 0 && recipe.Segs[n-1].HdrLen == 0 {
			recipe.Segs[n-1].RawLen += len(b)
		} else {
			recipe.Segs = append(recipe.Segs, MP4Seg{RawLen: len(b)})
		}
		rawBlob = append(rawBlob, b...)
	}
	for _, s := range samples {
		addRaw(orig[pos:s.off])
		// サンプル内: 長さ前置 NAL の列
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
			hdrRBSP, hdrBits, coded, err := eng.captureNAL(nal)
			if err != nil {
				return nil, false
			}
			if coded {
				recipe.Segs = append(recipe.Segs, MP4Seg{HdrLen: len(hdrRBSP), Bits: hdrBits, LenSize: tr.lenSize})
				hdrBlob = append(hdrBlob, hdrRBSP...)
			} else {
				// 長さ前置ごと原文で(SEI 等)
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
	if len(chunked)+len(recipe.Segs)*12+128 >= len(orig) {
		return nil, false
	}
	rt, err := ReconstructMP4H264(recipe, chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &MP4H264Unwrapped{Chunked: chunked, Recipe: recipe}, true
}

// ReconstructMP4H264 はレシピとチャンク化内容から元の MP4 をバイト単位で戻す。
func ReconstructMP4H264(recipe *MP4H264Recipe, chunked []byte) ([]byte, error) {
	if recipe == nil || len(recipe.Segs) == 0 ||
		recipe.RawN < 0 || recipe.HdrN < 0 || recipe.RawN+recipe.HdrN > len(chunked) {
		return nil, errH264BadRecipe
	}
	rawBlob := chunked[:recipe.RawN]
	hdrBlob := chunked[recipe.RawN : recipe.RawN+recipe.HdrN]
	arith := chunked[recipe.RawN+recipe.HdrN:]
	eng := newH264Rebuild(arith)
	// スケルトン(moov の avcC)から SPS/PPS を取り込む
	feedAvcC(rawBlob, eng)

	var out []byte
	rp, hp := 0, 0
	for _, sg := range recipe.Segs {
		if sg.RawLen > 0 && sg.HdrLen == 0 {
			if rp+sg.RawLen > len(rawBlob) {
				return nil, errH264BadRecipe
			}
			out = append(out, rawBlob[rp:rp+sg.RawLen]...)
			rp += sg.RawLen
			continue
		}
		if hp+sg.HdrLen > len(hdrBlob) || sg.LenSize < 1 || sg.LenSize > 4 {
			return nil, errH264BadRecipe
		}
		hdr := hdrBlob[hp : hp+sg.HdrLen]
		hp += sg.HdrLen
		nal, err := eng.rebuildNAL(hdr, sg.Bits)
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

// feedAvcC はバイト列から avcC パターンを探して SPS/PPS を取り込む
// (moov の位置に依存しない安直だが確実な走査)。
func feedAvcC(b []byte, eng *h264Rebuild) {
	for i := 0; i+8 < len(b); i++ {
		if b[i+4] != 'a' || b[i+5] != 'v' || b[i+6] != 'c' || b[i+7] != 'C' {
			continue
		}
		size := int(binary.BigEndian.Uint32(b[i:]))
		if size < 15 || i+size > len(b) {
			continue
		}
		avcC := b[i+8 : i+size]
		if len(avcC) < 7 {
			continue
		}
		p := 6
		nSPS := int(avcC[5] & 0x1F)
		for k := 0; k < nSPS && p+2 <= len(avcC); k++ {
			l := int(binary.BigEndian.Uint16(avcC[p:]))
			p += 2
			if p+l > len(avcC) {
				return
			}
			eng.observePS(avcC[p : p+l])
			p += l
		}
		if p >= len(avcC) {
			continue
		}
		nPPS := int(avcC[p])
		p++
		for k := 0; k < nPPS && p+2 <= len(avcC); k++ {
			l := int(binary.BigEndian.Uint16(avcC[p:]))
			p += 2
			if p+l > len(avcC) {
				return
			}
			eng.observePS(avcC[p : p+l])
			p += l
		}
	}
}
