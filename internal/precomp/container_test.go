//go:build cgo

package precomp

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"testing"
)

// zipMember はテスト用 ZIP の1メンバー。
type zipMember struct {
	name   string
	plain  []byte
	method uint16 // 0=stored, 8=deflate
	stream []byte // method=8 のときの生 deflate ストリーム
}

// buildZip はローカルヘッダ+セントラルディレクトリ+EOCD を手組みで並べた
// 最小の ZIP を作る(archive/zip の Writer は Go flate を使うため、
// zlib 産ストリームを埋め込むには手組みが必要)。
func buildZip(t *testing.T, members []zipMember) []byte {
	t.Helper()
	var out bytes.Buffer
	type cdent struct {
		m      zipMember
		offset uint32
	}
	var cd []cdent
	for _, m := range members {
		data := m.stream
		if m.method == 0 {
			data = m.plain
		}
		cd = append(cd, cdent{m: m, offset: uint32(out.Len())})
		out.WriteString("PK\x03\x04")
		w16 := func(v uint16) { binary.Write(&out, binary.LittleEndian, v) }
		w32 := func(v uint32) { binary.Write(&out, binary.LittleEndian, v) }
		w16(20)       // version needed
		w16(0)        // flags
		w16(m.method) // method
		w16(0)        // mod time
		w16(0x21)     // mod date(ゼロは不正な日付なので適当な値)
		w32(crc32.ChecksumIEEE(m.plain))
		w32(uint32(len(data)))
		w32(uint32(len(m.plain)))
		w16(uint16(len(m.name)))
		w16(0) // extra len
		out.WriteString(m.name)
		out.Write(data)
	}
	cdStart := out.Len()
	for _, e := range cd {
		data := e.m.stream
		if e.m.method == 0 {
			data = e.m.plain
		}
		out.WriteString("PK\x01\x02")
		w16 := func(v uint16) { binary.Write(&out, binary.LittleEndian, v) }
		w32 := func(v uint32) { binary.Write(&out, binary.LittleEndian, v) }
		w16(20) // version made by
		w16(20) // version needed
		w16(0)
		w16(e.m.method)
		w16(0)
		w16(0x21)
		w32(crc32.ChecksumIEEE(e.m.plain))
		w32(uint32(len(data)))
		w32(uint32(len(e.m.plain)))
		w16(uint16(len(e.m.name)))
		w16(0) // extra
		w16(0) // comment
		w16(0) // disk
		w16(0) // internal attr
		w32(0) // external attr
		w32(e.offset)
		out.WriteString(e.m.name)
	}
	cdSize := out.Len() - cdStart
	out.WriteString("PK\x05\x06")
	w16 := func(v uint16) { binary.Write(&out, binary.LittleEndian, v) }
	w32 := func(v uint32) { binary.Write(&out, binary.LittleEndian, v) }
	w16(0)
	w16(0)
	w16(uint16(len(cd)))
	w16(uint16(len(cd)))
	w32(uint32(cdSize))
	w32(uint32(cdStart))
	w16(0)
	return out.Bytes()
}

// zlibDeflate はシステム zlib 産の生 deflate ストリームを返す。
func zlibDeflate(t *testing.T, plain []byte, level int) []byte {
	t.Helper()
	stream, err := deflateExact(plain, level)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func testText(n int) []byte {
	var buf bytes.Buffer
	for buf.Len() < n {
		fmt.Fprintf(&buf, "container decomposition test line %d with shared vocabulary\n", buf.Len())
	}
	return buf.Bytes()[:n]
}

// zlib 産メンバーだけの ZIP は全メンバーが展開され、ビット一致で再構成できる。
func TestZipUnwrapRoundTrip(t *testing.T) {
	p1 := testText(200 << 10)
	p2 := bytes.Repeat([]byte("another member content! "), 8000)
	orig := buildZip(t, []zipMember{
		{name: "doc/a.xml", plain: p1, method: 8, stream: zlibDeflate(t, p1, 6)},
		{name: "doc/b.xml", plain: p2, method: 8, stream: zlibDeflate(t, p2, 9)},
		{name: "raw.bin", plain: []byte("stored member"), method: 0},
	})

	chunked, recipe, ok := TryUnwrapZip(orig, 0)
	if !ok {
		t.Fatal("ZIP の分解に失敗")
	}
	if len(recipe.Segments) != 2 {
		t.Fatalf("セグメント数 = %d, want 2", len(recipe.Segments))
	}
	// チャンク化内容には展開データ(圧縮前)が現れる
	if !bytes.Contains(chunked, p2[:100]) {
		t.Fatal("チャンク化内容に展開データが含まれていません")
	}
	rebuilt, err := ReconstructContainer(recipe, chunked)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rebuilt, orig) {
		t.Fatal("ZIP の再構成がビット一致しません")
	}
}

// zlib 産でないメンバーが混ざっていても、一致する分だけ部分適用される。
func TestZipUnwrapPartial(t *testing.T) {
	p1 := testText(100 << 10)
	p2 := testText(80 << 10)
	// p2 は Go の flate で圧縮(zlib とビット一致しない)
	var goStream bytes.Buffer
	fw, _ := flate.NewWriter(&goStream, 6)
	fw.Write(p2)
	fw.Close()

	orig := buildZip(t, []zipMember{
		{name: "zlib.xml", plain: p1, method: 8, stream: zlibDeflate(t, p1, 6)},
		{name: "goflate.xml", plain: p2, method: 8, stream: goStream.Bytes()},
	})

	chunked, recipe, ok := TryUnwrapZip(orig, 0)
	if !ok {
		t.Fatal("部分適用の分解に失敗")
	}
	if len(recipe.Segments) != 1 {
		t.Fatalf("セグメント数 = %d, want 1(zlib 産のみ)", len(recipe.Segments))
	}
	rebuilt, err := ReconstructContainer(recipe, chunked)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rebuilt, orig) {
		t.Fatal("部分適用の再構成がビット一致しません")
	}
}

// PDF の FlateDecode ストリームが分解・再構成できる。
func TestPDFUnwrapRoundTrip(t *testing.T) {
	content := testText(150 << 10)
	stream1, err := ReconstructZlib([]byte{0x78, 0x9c}, 6, content)
	if err != nil {
		t.Fatal(err)
	}
	content2 := bytes.Repeat([]byte("second pdf object data "), 4000)
	stream2, err := ReconstructZlib([]byte{0x78, 0xda}, 9, content2)
	if err != nil {
		t.Fatal(err)
	}
	var pdf bytes.Buffer
	fmt.Fprintf(&pdf, "%%PDF-1.4\n1 0 obj\n<</Length %d /Filter /FlateDecode>>\nstream\n", len(stream1))
	pdf.Write(stream1)
	pdf.WriteString("\nendstream\nendobj\n")
	fmt.Fprintf(&pdf, "2 0 obj\n<</Length %d /Filter /FlateDecode>>\nstream\r\n", len(stream2))
	pdf.Write(stream2)
	pdf.WriteString("\nendstream\nendobj\ntrailer\n<</Size 3>>\n%%EOF\n")
	orig := pdf.Bytes()

	chunked, recipe, ok := TryUnwrapPDF(orig, 0)
	if !ok {
		t.Fatal("PDF の分解に失敗")
	}
	if len(recipe.Segments) != 2 {
		t.Fatalf("セグメント数 = %d, want 2", len(recipe.Segments))
	}
	rebuilt, err := ReconstructContainer(recipe, chunked)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rebuilt, orig) {
		t.Fatal("PDF の再構成がビット一致しません")
	}
}

// 壊れた入力・zlib 産でない入力は素通し(false)になる。
func TestContainerRejectsGarbage(t *testing.T) {
	if _, _, ok := TryUnwrapZip([]byte("PK\x03\x04garbage-not-a-zip-file-really-long-enough-to-pass-min"), 0); ok {
		t.Fatal("壊れた ZIP を受理してしまいました")
	}
	if _, _, ok := TryUnwrapPDF([]byte("%PDF-1.4 no streams here, just text that is long enough ok"), 0); ok {
		t.Fatal("ストリームのない PDF を受理してしまいました")
	}
}

// 拡張パラメータ(memLevel 9 / Z_FILTERED)で作られた gzip も分解できる
// (従来は memLevel=8 / default strategy しか探索していなかった)。
func TestExtendedParamSearch(t *testing.T) {
	plain := testText(300 << 10)
	for _, tc := range []struct {
		name   string
		params int
	}{
		{"memLevel9", encodeParams(6, 9, 0)},
		{"filtered", encodeParams(9, 8, 1)},
		{"memLevel9-filtered-level1", encodeParams(1, 9, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := deflateExact(plain, tc.params)
			if err != nil {
				t.Fatal(err)
			}
			// 生成物が標準構成と本当に違うことを確認(探索拡張の意味の検証)
			std, _ := deflateExact(plain, encodeParams(6, 8, 0))
			if tc.name == "memLevel9" && bytes.Equal(stream, std) {
				t.Skip("この zlib では memLevel 9 の出力が標準と同一(探索不要)")
			}
			// gzip に包んで TryUnwrap
			var gz bytes.Buffer
			gz.Write([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3})
			gz.Write(stream)
			var tr [8]byte
			binary.LittleEndian.PutUint32(tr[:], crc32.ChecksumIEEE(plain))
			binary.LittleEndian.PutUint32(tr[4:], uint32(len(plain)))
			gz.Write(tr[:])

			u, ok := TryUnwrap(gz.Bytes(), 0)
			if !ok {
				t.Fatal("拡張パラメータの gzip を分解できません")
			}
			rebuilt, err := Reconstruct(u.Header, u.Level, u.Plain)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rebuilt, gz.Bytes()) {
				t.Fatal("再構成がビット一致しません")
			}
		})
	}
}

// パラメータ符号化の後方互換: 0..9 は素のレベルとして復号される。
func TestParamEncoding(t *testing.T) {
	for l := 0; l <= 9; l++ {
		if got := encodeParams(l, 8, 0); got != l {
			t.Fatalf("encodeParams(%d,8,0) = %d, want %d(後方互換)", l, got, l)
		}
		lv, m, st := decodeParams(l)
		if lv != l || m != 8 || st != 0 {
			t.Fatalf("decodeParams(%d) = (%d,%d,%d)", l, lv, m, st)
		}
	}
	v := encodeParams(9, 9, 1)
	lv, m, st := decodeParams(v)
	if lv != 9 || m != 9 || st != 1 {
		t.Fatalf("拡張符号の往復に失敗: (%d,%d,%d)", lv, m, st)
	}
}
