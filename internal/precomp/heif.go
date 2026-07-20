package precomp

// HEIF/HEIC(ISO/IEC 23008-12)静止画コンテナの可逆再圧縮。
//
// iPhone 標準の写真形式 HEIC は HEVC イントラを ISOBMFF の meta/iloc/iinf
// アイテム構造で格納する。ペイロード(長さ前置 NAL 列)は MP4 サンプルと
// 同型なので、iloc からアイテムのバイト範囲を求め hevcengine で I スライスを
// 再符号化する。骨格(ftyp/meta/mdat 中のアイテム外)は原文保持し、常に
// 全体を復元してバイト一致検証してから採用する(不一致・非対応は素通し)。
//
// iloc は version 0/1/2・construction_method・複数 extent があるが、
// 単一 hvc1 アイテム・construction_method 0(ファイルオフセット)の
// 標準構成のみ対象とし、それ以外は安全に素通しする。

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
)

// IsHEIF は HEIC/HEIF らしいか(ftyp のブランド)を判定する。
func IsHEIF(head []byte) bool {
	if len(head) < 12 || string(head[4:8]) != "ftyp" {
		return false
	}
	size := int(binary.BigEndian.Uint32(head[:4]))
	if size < 12 || size > len(head) {
		size = len(head)
	}
	// major_brand + compatible_brands を走査
	brands := [][]byte{head[8:12]}
	for p := 16; p+4 <= size; p += 4 {
		brands = append(brands, head[p:p+4])
	}
	for _, b := range brands {
		switch string(b) {
		case "heic", "heix", "heim", "heis", "hevc", "mif1", "msf1", "heifs":
			return true
		}
	}
	return false
}

// heifItem は 1 アイテムのバイト範囲(ファイル絶対、単一 extent)。
type heifItem struct {
	id     int
	offset int64
	length int64
}

// heifInfo は HEIF 解析結果。
type heifInfo struct {
	items   []heifItem // hvc1/hvc2 アイテム(ファイルオフセット順)
	psNALs  [][]byte   // hvcC 由来のパラメータセット
	lenSize int
}

// parseHEIF は meta の iinf/iloc/iprp を解析し、HEVC アイテムの範囲と
// パラメータセットを返す。
func parseHEIF(file []byte) (*heifInfo, bool) {
	meta := findBox(file, "meta")
	if meta == nil || len(meta) < 4 {
		return nil, false
	}
	metaBody := meta[4:] // meta は fullbox(version+flags 4B)
	// iinf: hvc1/hvc2 アイテム ID の収集
	hvcItems := map[int]bool{}
	iinf := findBox(metaBody, "iinf")
	if iinf == nil {
		return nil, false
	}
	if !heifParseIinf(iinf, hvcItems) {
		return nil, false
	}
	if len(hvcItems) == 0 {
		return nil, false
	}
	// iprp/ipco/hvcC: パラメータセット(最初の hvcC を使う)
	var ps [][]byte
	lenSize := 4
	if iprp := findBox(metaBody, "iprp"); iprp != nil {
		if ipco := findBox(iprp, "ipco"); ipco != nil {
			mp4Boxes(ipco, func(t string, body []byte) bool {
				if t == "hvcC" && ps == nil {
					if p, ls, ok := parseHvcC(body); ok {
						ps = p
						lenSize = ls
					}
				}
				return true
			})
		}
	}
	if ps == nil {
		return nil, false
	}
	// iloc: アイテムのバイト範囲
	iloc := findBox(metaBody, "iloc")
	if iloc == nil {
		return nil, false
	}
	locs, ok := heifParseIloc(iloc)
	if !ok {
		return nil, false
	}
	info := &heifInfo{psNALs: ps, lenSize: lenSize}
	for id := range hvcItems {
		l, ok := locs[id]
		if !ok {
			return nil, false
		}
		info.items = append(info.items, heifItem{id: id, offset: l.offset, length: l.length})
	}
	// オフセット順に整列
	for i := 1; i < len(info.items); i++ {
		for j := i; j > 0 && info.items[j-1].offset > info.items[j].offset; j-- {
			info.items[j-1], info.items[j] = info.items[j], info.items[j-1]
		}
	}
	return info, true
}

// heifParseIinf は iinf(fullbox)から hvc1/hvc2 アイテム ID を集める。
func heifParseIinf(iinf []byte, out map[int]bool) bool {
	if len(iinf) < 4 {
		return false
	}
	ver := iinf[0]
	p := 4
	// entry_count: version 0 は 16bit、それ以外 32bit
	if ver == 0 {
		if p+2 > len(iinf) {
			return false
		}
		p += 2
	} else {
		if p+4 > len(iinf) {
			return false
		}
		p += 4
	}
	// 続く infe ボックス群
	mp4Boxes(iinf[p:], func(t string, body []byte) bool {
		if t != "infe" || len(body) < 4 {
			return true
		}
		iver := body[0]
		q := 4
		var itemID int
		if iver >= 2 {
			if iver == 2 {
				if q+2 > len(body) {
					return true
				}
				itemID = int(binary.BigEndian.Uint16(body[q:]))
				q += 2
			} else { // version 3
				if q+4 > len(body) {
					return true
				}
				itemID = int(binary.BigEndian.Uint32(body[q:]))
				q += 4
			}
			q += 2 // item_protection_index
			if q+4 > len(body) {
				return true
			}
			itype := string(body[q : q+4])
			if itype == "hvc1" || itype == "hvc2" {
				out[itemID] = true
			}
		}
		return true
	})
	return true
}

type ilocEntry struct {
	offset int64
	length int64
}

// heifParseIloc は iloc(fullbox)を解析する。construction_method 0・
// 単一 extent のみ対象(それ以外は false)。
func heifParseIloc(iloc []byte) (map[int]ilocEntry, bool) {
	if len(iloc) < 8 {
		return nil, false
	}
	ver := iloc[0]
	p := 4
	offSize := int(iloc[p] >> 4)
	lenSize := int(iloc[p] & 0xF)
	baseSize := int(iloc[p+1] >> 4)
	indexSize := 0
	p += 2
	if ver == 1 || ver == 2 {
		indexSize = int(iloc[p-1] & 0xF)
	}
	var itemCount int
	if ver < 2 {
		if p+2 > len(iloc) {
			return nil, false
		}
		itemCount = int(binary.BigEndian.Uint16(iloc[p:]))
		p += 2
	} else {
		if p+4 > len(iloc) {
			return nil, false
		}
		itemCount = int(binary.BigEndian.Uint32(iloc[p:]))
		p += 4
	}
	readN := func(n int) (int64, bool) {
		if n == 0 {
			return 0, true
		}
		if p+n > len(iloc) {
			return 0, false
		}
		var v int64
		for i := 0; i < n; i++ {
			v = v<<8 | int64(iloc[p+i])
		}
		p += n
		return v, true
	}
	out := map[int]ilocEntry{}
	for i := 0; i < itemCount; i++ {
		var itemID int
		if ver < 2 {
			if p+2 > len(iloc) {
				return nil, false
			}
			itemID = int(binary.BigEndian.Uint16(iloc[p:]))
			p += 2
		} else {
			if p+4 > len(iloc) {
				return nil, false
			}
			itemID = int(binary.BigEndian.Uint32(iloc[p:]))
			p += 4
		}
		if ver == 1 || ver == 2 {
			// reserved(12) + construction_method(4)
			if p+2 > len(iloc) {
				return nil, false
			}
			cm := int(iloc[p+1] & 0xF)
			p += 2
			if cm != 0 {
				return nil, false // ファイルオフセット以外は対象外
			}
		}
		p += 2 // data_reference_index
		base, ok := readN(baseSize)
		if !ok {
			return nil, false
		}
		if p+2 > len(iloc) {
			return nil, false
		}
		extentCount := int(binary.BigEndian.Uint16(iloc[p:]))
		p += 2
		if extentCount != 1 {
			return nil, false // 単一 extent のみ
		}
		if indexSize > 0 {
			if _, ok := readN(indexSize); !ok {
				return nil, false
			}
		}
		off, ok := readN(offSize)
		if !ok {
			return nil, false
		}
		ln, ok := readN(lenSize)
		if !ok {
			return nil, false
		}
		out[itemID] = ilocEntry{offset: base + off, length: ln}
	}
	return out, true
}

// HEIFRecipe は HEIF 再構成レシピ(MP4H264 と同型)。
type HEIFRecipe struct {
	Segs []MP4Seg `json:"segs"`
	RawN int      `json:"rn,omitempty"`
	HdrN int      `json:"hn,omitempty"`
}

// HEIFUnwrapped は分解結果。
type HEIFUnwrapped struct {
	Chunked []byte
	Recipe  *HEIFRecipe
}

// TryUnwrapHEIF は HEIC 内の HEVC イントラアイテムを再符号化する。
func TryUnwrapHEIF(orig []byte, maxPlain int64) (*HEIFUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 64 || !IsHEIF(orig) {
		return nil, false
	}
	info, ok := parseHEIF(orig)
	if !ok || len(info.items) == 0 {
		return nil, false
	}
	// アイテム範囲の妥当性(重複・範囲外は対象外)
	var last int64
	for _, it := range info.items {
		if it.offset < last || it.length < 0 || it.offset+it.length > int64(len(orig)) {
			return nil, false
		}
		last = it.offset + it.length
	}

	eng := newHEVCCapture()
	for _, ps := range info.psNALs {
		eng.observePS(ps)
	}

	recipe := &HEIFRecipe{}
	var rawBlob, hdrBlob []byte
	pos := int64(0)
	addRaw := func(b []byte) {
		if len(b) == 0 {
			return
		}
		eng.picOK = false
		if n := len(recipe.Segs); n > 0 && recipe.Segs[n-1].RawLen > 0 && recipe.Segs[n-1].HdrLen == 0 {
			recipe.Segs[n-1].RawLen += len(b)
		} else {
			recipe.Segs = append(recipe.Segs, MP4Seg{RawLen: len(b)})
		}
		rawBlob = append(rawBlob, b...)
	}
	for _, it := range info.items {
		addRaw(orig[pos:it.offset])
		p := it.offset
		end := it.offset + it.length
		for p < end {
			if p+int64(info.lenSize) > end {
				return nil, false
			}
			var l int64
			for i := 0; i < info.lenSize; i++ {
				l = l<<8 | int64(orig[p+int64(i)])
			}
			p += int64(info.lenSize)
			if l <= 0 || p+l > end {
				return nil, false
			}
			nal := orig[p : p+l]
			blob, hdrBits, coded, err := eng.captureNAL(nal)
			if err != nil {
				return nil, false
			}
			if coded {
				recipe.Segs = append(recipe.Segs, MP4Seg{HdrLen: len(blob), Bits: hdrBits, LenSize: info.lenSize})
				hdrBlob = append(hdrBlob, blob...)
			} else {
				addRaw(orig[p-int64(info.lenSize) : p+l])
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
	rt, err := ReconstructHEIF(recipe, chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &HEIFUnwrapped{Chunked: chunked, Recipe: recipe}, true
}

// ReconstructHEIF はレシピとチャンク化内容から元の HEIC を戻す。
func ReconstructHEIF(recipe *HEIFRecipe, chunked []byte) ([]byte, error) {
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
			eng.picOK = false
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
