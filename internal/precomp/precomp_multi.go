//go:build cgo

package precomp

// 生 zlib ストリームとマルチメンバー gzip(連結 gzip)への拡張。
//
// - 生 zlib(0x78 等のマジック): git の loose オブジェクト、PDF の
//   FlateDecode、PNG IDAT など多くのフォーマットの内部で使われる。
//   構造は 2バイトヘッダ + deflate + adler32(4バイト, big-endian)。
// - マルチメンバー gzip: ローテートされたログの連結(cat a.gz b.gz)や
//   bgzip(BGZF)などで現れる。メンバーごとにヘッダ+deflate+トレーラが並ぶ。
//
// どちらも既存の gzip 対応と同じ「zlib レベル探索+ビット一致検証」方式。

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/adler32"
	"hash/crc32"
	"io"
)

const (
	// minStreamSize 未満の入力は分解を試みない(誤検出対策)。
	minStreamSize = 64
	// maxMembers を超えるマルチメンバー gzip は分解しない
	// (BGZF のような64KiB刻みの巨大ファイルでレベル探索が暴走しないように)。
	maxMembers = 1024
	// maxPlainTotal を超える展開は打ち切る(zip bomb 対策・メモリ上限)。
	maxPlainTotal = 1 << 30
)

// IsZlib は zlib ストリームのマジック(CMF/FLG)かを判定する。
// deflate 方式・FDICT なし・チェックサム条件を満たす場合のみ真。
func IsZlib(head []byte) bool {
	if len(head) < 2 {
		return false
	}
	cmf, flg := head[0], head[1]
	return cmf&0x0f == 8 && // 圧縮方式 = deflate
		cmf>>4 <= 7 && // 窓サイズが規格内
		flg&0x20 == 0 && // FDICT なし
		(uint32(cmf)*256+uint32(flg))%31 == 0
}

// TryUnwrapZlib は生 zlib ストリームを「展開データ+レシピ」に分解する。
// maxPlain は展開データの上限(0 ならデフォルト上限)。
func TryUnwrapZlib(orig []byte, maxPlain int64) (*Unwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if len(orig) < minStreamSize || !IsZlib(orig) {
		return nil, false
	}
	deflateStream := orig[2 : len(orig)-4]
	trailer := orig[len(orig)-4:]

	fr := flate.NewReader(bytes.NewReader(deflateStream))
	plain, err := io.ReadAll(io.LimitReader(fr, maxPlain+1))
	fr.Close()
	if err != nil || int64(len(plain)) > maxPlain {
		return nil, false
	}
	if adler32.Checksum(plain) != binary.BigEndian.Uint32(trailer) {
		return nil, false
	}
	if level, ok := findLevel(plain, deflateStream); ok {
		header := append([]byte(nil), orig[:2]...)
		return &Unwrapped{Header: header, Plain: plain, Level: level}, true
	}
	return nil, false
}

// ReconstructZlib はレシピから元の zlib ストリームを再構成する。
func ReconstructZlib(header []byte, level int, plain []byte) ([]byte, error) {
	stream, err := deflateExact(plain, level)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(header)+len(stream)+4)
	out = append(out, header...)
	out = append(out, stream...)
	var trailer [4]byte
	binary.BigEndian.PutUint32(trailer[:], adler32.Checksum(plain))
	return append(out, trailer[:]...), nil
}

// Member はマルチメンバー gzip の1メンバーのレシピ。
type Member struct {
	// Header はメンバーの gzip ヘッダ原文。
	Header []byte `json:"header"`
	// Level は zlib 圧縮レベル。
	Level int `json:"level"`
	// PlainLen はこのメンバーの展開データ長(連結展開データの分割に使う)。
	PlainLen int64 `json:"plain_len"`
}

// TryUnwrapGzipMulti は(マルチメンバーの可能性がある)gzip ストリームを
// 「連結展開データ+メンバーレシピ列」に分解する。全メンバーが zlib 産で
// ビット一致再現できる場合のみ成功する。maxPlain は展開合計の上限
// (0 ならデフォルト上限)。
func TryUnwrapGzipMulti(orig []byte, maxPlain int64) ([]byte, []Member, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if len(orig) < minStreamSize || !IsGzip(orig) {
		return nil, nil, false
	}
	var plain []byte
	var members []Member
	pos := 0
	for pos < len(orig) {
		if len(members) >= maxMembers || !IsGzip(orig[pos:]) {
			return nil, nil, false
		}
		hl, err := parseHeaderLen(orig[pos:])
		if err != nil || pos+hl+8 > len(orig) {
			return nil, nil, false
		}
		// bytes.Reader は io.ByteReader なので flate は必要以上に読まない
		// → 消費バイト数から deflate ストリームの範囲を特定できる。
		br := bytes.NewReader(orig[pos+hl:])
		fr := flate.NewReader(br)
		part, err := io.ReadAll(io.LimitReader(fr, maxPlain+1))
		fr.Close()
		if err != nil || int64(len(plain))+int64(len(part)) > maxPlain {
			return nil, nil, false
		}
		consumed := len(orig) - pos - hl - br.Len()
		deflateStream := orig[pos+hl : pos+hl+consumed]
		trailerPos := pos + hl + consumed
		if trailerPos+8 > len(orig) {
			return nil, nil, false
		}
		trailer := orig[trailerPos : trailerPos+8]
		if crc32.ChecksumIEEE(part) != binary.LittleEndian.Uint32(trailer) ||
			uint32(len(part)) != binary.LittleEndian.Uint32(trailer[4:]) {
			return nil, nil, false
		}
		level, ok := findLevel(part, deflateStream)
		if !ok {
			return nil, nil, false
		}
		members = append(members, Member{
			Header:   append([]byte(nil), orig[pos:pos+hl]...),
			Level:    level,
			PlainLen: int64(len(part)),
		})
		plain = append(plain, part...)
		pos = trailerPos + 8
	}
	if len(members) == 0 {
		return nil, nil, false
	}
	return plain, members, true
}

// ReconstructGzipMulti はメンバーレシピ列から元の gzip バイト列を再構成する。
func ReconstructGzipMulti(members []Member, plain []byte) ([]byte, error) {
	var out []byte
	off := int64(0)
	for _, mb := range members {
		if off+mb.PlainLen > int64(len(plain)) {
			return nil, fmt.Errorf("展開データ長がレシピと一致しません")
		}
		part := plain[off : off+mb.PlainLen]
		rec, err := Reconstruct(mb.Header, mb.Level, part)
		if err != nil {
			return nil, err
		}
		out = append(out, rec...)
		off += mb.PlainLen
	}
	if off != int64(len(plain)) {
		return nil, fmt.Errorf("展開データに余りがあります")
	}
	return out, nil
}
