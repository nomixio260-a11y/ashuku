package precomp

// MP4/M4A(mp4a)入り AAC-LC の可逆再圧縮。音楽アプリ・スマホ動画の音声
// トラックの定番。stsd の mp4a サンプルエントリと esds(AudioSpecificConfig)
// から sample_frequency_index を取り出し、サンプル(=raw_data_block)を
// aacengine で再符号化する。骨格(MP4 ボックス+非対応サンプル)は原文保持。

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"sort"
)

// mp4AudioTrack は 1 音声トラックのサンプル表。
type mp4AudioTrack struct {
	sfi     int
	objType int
	sizes   []int
	offsets []int64
}

// findMoovContent は 'moov' ボックス署名を走査して中身(ヘッダ後)を返す。
// 骨格は原文からサンプルバイトを抜くため mdat のサイズが実長と食い違い
// 全体をボックス列として整列できないので、moov だけを署名で見つける。
func findMoovContent(data []byte) []byte {
	for i := 4; i+4 <= len(data); i++ {
		if data[i] != 'm' || data[i+1] != 'o' || data[i+2] != 'o' || data[i+3] != 'v' {
			continue
		}
		size := int(binary.BigEndian.Uint32(data[i-4 : i]))
		if size >= 16 && i-4+size <= len(data) {
			return data[i+4 : i-4+size]
		}
	}
	return nil
}

// parseMP4AudioTrack は mp4a(AAC-LC)トラックを取り出す(1 本のみ対応)。
func parseMP4AudioTrack(file []byte) (*mp4AudioTrack, bool) {
	moov := findMoovContent(file)
	if moov == nil {
		return nil, false
	}
	var tr *mp4AudioTrack
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
		if binary.BigEndian.Uint32(stsd[4:8]) != 1 {
			return true
		}
		entry := stsd[8:]
		if len(entry) < 8 || string(entry[4:8]) != "mp4a" {
			return true
		}
		if tr != nil { // 複数音声トラックは対象外
			okAll = false
			return false
		}
		// AudioSampleEntry 固定長(8 sampleEntry + 20 audio)後に子ボックス
		var esds []byte
		if len(entry) >= 8+28 {
			mp4Boxes(entry[8+28:], func(ct string, cb []byte) bool {
				if ct == "esds" && esds == nil {
					esds = cb
				}
				return true
			})
		}
		if esds == nil {
			return true
		}
		aot, sfi, ok := parseESDS(esds)
		if !ok || aot != 2 { // AAC-LC のみ
			return true
		}
		t2 := &mp4AudioTrack{sfi: sfi, objType: aot}
		vt := &mp4Track{}
		if !mp4SampleTable(stbl, vt, false) {
			return true
		}
		t2.sizes = vt.sizes
		t2.offsets = vt.offsets
		tr = t2
		return true
	})
	if !okAll || tr == nil || len(tr.offsets) == 0 {
		return nil, false
	}
	return tr, true
}

// parseESDS は esds(fullbox)から AudioSpecificConfig の
// audioObjectType と sampling_frequency_index を取り出す。
func parseESDS(esds []byte) (int, int, bool) {
	if len(esds) < 4 {
		return 0, 0, false
	}
	p := 4 // version+flags
	readLen := func() int {
		l := 0
		for i := 0; i < 4 && p < len(esds); i++ {
			b := esds[p]
			p++
			l = l<<7 | int(b&0x7f)
			if b&0x80 == 0 {
				break
			}
		}
		return l
	}
	if p >= len(esds) || esds[p] != 0x03 {
		return 0, 0, false
	}
	p++
	readLen()
	p += 2 // ES_ID
	if p >= len(esds) {
		return 0, 0, false
	}
	flags := esds[p]
	p++
	if flags&0x80 != 0 {
		p += 2
	}
	if flags&0x40 != 0 {
		if p >= len(esds) {
			return 0, 0, false
		}
		ul := int(esds[p])
		p += 1 + ul
	}
	if flags&0x20 != 0 {
		p += 2
	}
	if p >= len(esds) || esds[p] != 0x04 {
		return 0, 0, false
	}
	p++
	readLen()
	p += 1 + 1 + 3 + 4 + 4 // OTI+streamType+bufferSize+max+avg
	if p >= len(esds) || esds[p] != 0x05 {
		return 0, 0, false
	}
	p++
	ascLen := readLen()
	if ascLen < 2 || p+2 > len(esds) {
		return 0, 0, false
	}
	aot := int(esds[p] >> 3)
	sfi := int(esds[p]&0x7)<<1 | int(esds[p+1]>>7)
	return aot, sfi, true
}

// M4ARecipe は MP4(AAC)再構成レシピ。サンプル境界・sfi は骨格中の
// moov(esds/stsz/stco/stsc)から復元時に再解析するので、レシピは骨格長と
// 原文退避サンプル番号のみ(通常は空)。
type M4ARecipe struct {
	RawN     int   `json:"rn"`
	Verbatim []int `json:"vb,omitempty"`
}

// M4AUnwrapped は分解結果。
type M4AUnwrapped struct {
	Chunked []byte
	Recipe  *M4ARecipe
}

// TryUnwrapM4A は MP4 内の AAC-LC サンプルを再符号化する。
func TryUnwrapM4A(orig []byte, maxPlain int64) (*M4AUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 64 || !IsMP4(orig) {
		return nil, false
	}
	tr, ok := parseMP4AudioTrack(orig)
	if !ok {
		return nil, false
	}
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

	aacInitDec()
	enc := newRangeEncoder()
	models := newAACModels()
	recipe := &M4ARecipe{}
	var skel []byte
	pos := int64(0)
	coded := 0
	for i, s := range samples {
		skel = append(skel, orig[pos:s.off]...) // サンプル間の非サンプルバイト
		body := orig[s.off : s.off+int64(s.size)]
		vw := &h264Writer{}
		vs := &aacCaptureSink{r: &h264Reader{b: body}, cw: vw, m: models}
		if aacWalkRDB(vs, tr.sfi) && !vs.bad && vs.r.pos == len(body)*8 && bytes.Equal(vw.b, body) {
			es := &aacCaptureSink{r: &h264Reader{b: body}, cw: &h264Writer{}, enc: enc, m: models}
			aacWalkRDB(es, tr.sfi)
			if es.bad {
				return nil, false
			}
			coded++ // 符号化サンプルは骨格に入れない
		} else {
			skel = append(skel, body...) // 原文退避
			recipe.Verbatim = append(recipe.Verbatim, i)
		}
		pos = s.off + int64(s.size)
	}
	skel = append(skel, orig[pos:]...)
	if coded == 0 {
		return nil, false
	}
	arith := enc.finish()
	recipe.RawN = len(skel)
	chunked := make([]byte, 0, len(skel)+len(arith))
	chunked = append(chunked, skel...)
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
	rt, err := ReconstructM4A(recipe, chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &M4AUnwrapped{Chunked: chunked, Recipe: recipe}, true
}

// ReconstructM4A はレシピと Chunked から元の MP4 を戻す。サンプル境界は
// 骨格中の moov から再解析し、符号化サンプルを算術から復元して差し込む。
func ReconstructM4A(recipe *M4ARecipe, chunked []byte) ([]byte, error) {
	if recipe == nil || recipe.RawN < 0 || recipe.RawN > len(chunked) {
		return nil, errH264BadRecipe
	}
	aacInitDec()
	skel := chunked[:recipe.RawN]
	arith := chunked[recipe.RawN:]
	tr, ok := parseMP4AudioTrack(skel)
	if !ok {
		return nil, errH264BadRecipe
	}
	type sample struct {
		off  int64
		size int
	}
	samples := make([]sample, len(tr.offsets))
	for i := range tr.offsets {
		samples[i] = sample{tr.offsets[i], tr.sizes[i]}
	}
	sort.Slice(samples, func(a, b int) bool { return samples[a].off < samples[b].off })
	vb := map[int]bool{}
	for _, i := range recipe.Verbatim {
		vb[i] = true
	}
	dec := newRangeDecoder(arith)
	models := newAACModels()
	var out []byte
	sp := 0 // 骨格読み位置
	origPos := int64(0)
	// 退避サンプルはソート後の位置で識別する(TryUnwrapM4A も同じ順で記録)。
	// off は検証済みで厳密増加ゆえ、両者の sort 結果は一致する。
	for i, s := range samples {
		gap := int(s.off - origPos) // 非サンプルバイト
		if gap < 0 || sp+gap > len(skel) {
			return nil, errH264BadRecipe
		}
		out = append(out, skel[sp:sp+gap]...)
		sp += gap
		origPos = s.off
		if vb[i] {
			if sp+s.size > len(skel) {
				return nil, errH264BadRecipe
			}
			out = append(out, skel[sp:sp+s.size]...)
			sp += s.size
		} else {
			cw := &h264Writer{}
			rs := &aacRebuildSink{dec: dec, cw: cw, m: models}
			if !aacWalkRDB(rs, tr.sfi) || rs.bad || len(cw.b) != s.size {
				return nil, errH264BadRecipe
			}
			out = append(out, cw.b...)
		}
		origPos += int64(s.size)
	}
	// 末尾の非サンプルバイト
	out = append(out, skel[sp:]...)
	return out, nil
}
