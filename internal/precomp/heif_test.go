package precomp

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// buildBox は ISOBMFF ボックスを組む。
func buildBox(typ string, body []byte) []byte {
	b := make([]byte, 0, 8+len(body))
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(8+len(body)))
	b = append(b, sz[:]...)
	b = append(b, typ...)
	b = append(b, body...)
	return b
}

func u16b(v int) []byte { var b [2]byte; binary.BigEndian.PutUint16(b[:], uint16(v)); return b[:] }
func u32b(v int) []byte { var b [4]byte; binary.BigEndian.PutUint32(b[:], uint32(v)); return b[:] }

// buildSyntheticHEIF は HEVC Annex B(単一 I ピクチャ)から最小の HEIC を組む。
func buildSyntheticHEIF(t *testing.T, annexb []byte) []byte {
	nals, ok := splitAnnexB(annexb)
	if !ok {
		t.Fatal("split 失敗")
	}
	var vps, sps, pps []byte
	var sliceNALs [][]byte
	for _, n := range nals {
		switch hevcNALType(n.data) {
		case hevcNALVPS:
			vps = n.data
		case hevcNALSPS:
			sps = n.data
		case hevcNALPPS:
			pps = n.data
		default:
			if hevcIsSlice(hevcNALType(n.data)) {
				sliceNALs = append(sliceNALs, n.data)
			}
		}
	}
	if vps == nil || sps == nil || pps == nil || len(sliceNALs) == 0 {
		t.Fatal("PS/スライス不足")
	}
	// hvcC(lengthSizeMinusOne=3)
	hvcC := []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	hvcC = append(hvcC, 0xff) // lengthSizeMinusOne=3 (下位2bit)
	hvcC = append(hvcC, 3)    // numOfArrays
	arr := func(nalType int, nal []byte) []byte {
		a := []byte{byte(nalType)}
		a = append(a, u16b(1)...) // numNalus
		a = append(a, u16b(len(nal))...)
		a = append(a, nal...)
		return a
	}
	hvcC = append(hvcC, arr(hevcNALVPS, vps)...)
	hvcC = append(hvcC, arr(hevcNALSPS, sps)...)
	hvcC = append(hvcC, arr(hevcNALPPS, pps)...)

	// mdat: アイテムデータ = 長さ前置(4B)スライス NAL 列
	var itemData []byte
	for _, s := range sliceNALs {
		itemData = append(itemData, u32b(len(s))...)
		itemData = append(itemData, s...)
	}

	// ftyp
	ftyp := buildBox("ftyp", append([]byte("heic"+"\x00\x00\x00\x00"), []byte("mif1heic")...))

	// meta の子ボックス
	hdlr := buildBox("hdlr", append(make([]byte, 8), append([]byte("pict"), make([]byte, 13)...)...))
	pitm := buildBox("pitm", append([]byte{0, 0, 0, 0}, u16b(1)...)) // version0, item_ID=1
	// iinf/infe(version2, item_ID=1, type hvc1)
	infeBody := []byte{2, 0, 0, 0}
	infeBody = append(infeBody, u16b(1)...) // item_ID
	infeBody = append(infeBody, u16b(0)...) // protection_index
	infeBody = append(infeBody, []byte("hvc1")...)
	infeBody = append(infeBody, 0) // item_name(空文字列)
	infe := buildBox("infe", infeBody)
	iinfBody := []byte{0, 0, 0, 0}
	iinfBody = append(iinfBody, u16b(1)...) // entry_count(version0=16bit)
	iinfBody = append(iinfBody, infe...)
	iinf := buildBox("iinf", iinfBody)
	// iprp/ipco/hvcC + ispe
	ispe := buildBox("ispe", append([]byte{0, 0, 0, 0}, append(u32b(1024), u32b(768)...)...))
	ipco := buildBox("ipco", append(buildBox("hvcC", hvcC), ispe...))
	// ipma: item1 → 2 プロパティ(1-based index)
	ipmaBody := []byte{0, 0, 0, 0}
	ipmaBody = append(ipmaBody, u32b(1)...) // entry_count
	ipmaBody = append(ipmaBody, u16b(1)...) // item_ID
	ipmaBody = append(ipmaBody, 2)          // association_count
	ipmaBody = append(ipmaBody, 0x81, 0x02) // essential+prop1, prop2
	ipma := buildBox("ipma", ipmaBody)
	iprp := buildBox("iprp", append(ipco, ipma...))

	// meta 本体(iloc は後で offset 確定後に差し込むためプレースホルダ)
	// レイアウト: [ftyp][meta][mdat]。mdat データ開始オフセットを計算するため、
	// meta サイズを iloc 込みで確定する必要がある。iloc は固定長なので先に組む。
	// iloc(version1, offset_size=4,length_size=4,base_offset_size=0,index_size=0)
	buildIloc := func(mdatDataOff int) []byte {
		body := []byte{1, 0, 0, 0}                // version1
		body = append(body, 0x44)                 // offset_size=4, length_size=4
		body = append(body, 0x00)                 // base_offset_size=0, index_size=0
		body = append(body, u16b(1)...)           // item_count
		body = append(body, u16b(1)...)           // item_ID
		body = append(body, u16b(0)...)           // reserved(12)+construction_method(4)=0
		body = append(body, u16b(0)...)           // data_reference_index
		body = append(body, u16b(1)...)           // extent_count
		body = append(body, u32b(mdatDataOff)...) // extent_offset
		body = append(body, u32b(len(itemData))...)
		return buildBox("iloc", body)
	}
	metaFixed := append(append(append(hdlr, pitm...), iinf...), iprp...)
	// meta = fullbox(4) + hdlr + pitm + iinf + iprp + iloc
	ilocLen := len(buildIloc(0))
	metaContentLen := 4 + len(metaFixed) + ilocLen
	metaBoxLen := 8 + metaContentLen
	mdatDataOff := len(ftyp) + metaBoxLen + 8 // +8 = mdat ヘッダ
	iloc := buildIloc(mdatDataOff)
	metaBody := append([]byte{0, 0, 0, 0}, append(metaFixed, iloc...)...)
	meta := buildBox("meta", metaBody)
	mdat := buildBox("mdat", itemData)

	out := append(append(ftyp, meta...), mdat...)
	return out
}

func TestHEIFRoundTrip(t *testing.T) {
	// photo_1080p は単一 I ピクチャの HEVC。それを HEIC に包む。
	annexb, err := os.ReadFile("testdata/hevc/photo_1080p.h265")
	if err != nil {
		t.Skip(err)
	}
	heic := buildSyntheticHEIF(t, annexb)
	if !IsHEIF(heic) {
		t.Fatal("IsHEIF=false")
	}
	info, ok := parseHEIF(heic)
	if !ok {
		t.Fatal("parseHEIF 失敗")
	}
	t.Logf("parseHEIF: items=%d lenSize=%d ps=%d", len(info.items), info.lenSize, len(info.psNALs))
	uw, ok := TryUnwrapHEIF(heic, 0)
	if !ok {
		t.Fatal("TryUnwrapHEIF 不採用")
	}
	rt, err := ReconstructHEIF(uw.Recipe, uw.Chunked)
	if err != nil || !bytes.Equal(rt, heic) {
		t.Fatalf("往復不一致 err=%v", err)
	}
	zo := jpegProbeEncoder.EncodeAll(heic, nil)
	zc := jpegProbeEncoder.EncodeAll(uw.Chunked, nil)
	t.Logf("HEIC %dB→%dB zstd比 %d→%d(%.2f%%)", len(heic), len(uw.Chunked), len(zo), len(zc),
		100*(float64(len(zc))-float64(len(zo)))/float64(len(zo)))
}
