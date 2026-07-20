//go:build cgo

package precomp

// コンテナ形式(ZIP / PDF)の分解。
//
// ZIP(docx/xlsx/pptx/jar/apk を含む)と PDF は「メタ構造の中に deflate/zlib
// ストリームが埋まっている」コンテナで、外側から zstd をかけてもほぼ縮まない。
// そこで:
//
//   - ZIP: セントラルディレクトリを読み、deflate メンバー(method 8)の
//     生 deflate ストリームを特定する。
//   - PDF: "stream" トークンを走査し、直後の zlib ストリーム(FlateDecode)を
//     消費バイト追跡つき inflate で特定する。
//
// 各ストリームは zlib レベル探索でビット一致再現できる場合のみ抜き出し、
// 「スケルトン(ストリームを除いた原文)+ 展開データ列」に分解する。
// 一致しないストリームはスケルトンに残す(部分適用できるのが gzip 系と違う
// 強み: エンコーダ不明のメンバーが混ざっていても、一致する分だけ得をする)。
//
// チャンク化される内容は skeleton || plains の連結。スケルトン(ZIP の
// ヘッダ・無圧縮メンバー・PDF のテキスト部)も dedup/zstd の対象になるため、
// 「ほぼ同じ ZIP の別世代」はチャンクレベルで重複排除が効くようになる。
//
// 再構成はレシピどおりにスケルトンへ deflateExact の出力を継ぎ足す。
// 分解時に全体をビット一致で自己検証してから採用する(検証に失敗したら
// 何もなかったことにして素通しする)ため、可逆性は保存前に保証される。

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/adler32"
	"io"
	"sort"
)

// IsZip は ZIP のローカルファイルヘッダ署名かを返す。
func IsZip(head []byte) bool {
	return len(head) >= 4 && head[0] == 'P' && head[1] == 'K' && head[2] == 3 && head[3] == 4
}

// IsPDF は PDF 署名かを返す。
func IsPDF(head []byte) bool {
	return len(head) >= 5 && string(head[:5]) == "%PDF-"
}

// ContainerSegment は抜き出した1ストリームのレシピ。
type ContainerSegment struct {
	// SkelPos はスケルトン内の挿入位置(この位置にストリームを再挿入する)。
	SkelPos int64 `json:"skel_pos"`
	// PlainLen はこのストリームの展開データ長(連結展開データの分割に使う)。
	PlainLen int64 `json:"plain_len"`
	// Level は zlib 圧縮レベル。
	Level int `json:"level"`
	// Header は zlib ラッパの2バイトヘッダ(ZIP の生 deflate では空)。
	// 非空なら zlib ストリーム(header + deflate + adler32)として再構成する。
	Header []byte `json:"header,omitempty"`
}

// ContainerRecipe はコンテナ分解の再構成レシピ。
type ContainerRecipe struct {
	// SkelLen はチャンク化内容の先頭を占めるスケルトンの長さ。
	SkelLen int64 `json:"skel_len"`
	// Segments はスケルトン位置の昇順に並んだストリームレシピ。
	Segments []ContainerSegment `json:"segments"`
}

// rawSegment は分解中の中間表現(元ファイル内の位置で保持)。
type rawSegment struct {
	origOff int64 // 元ファイル内のストリーム開始位置
	origLen int64 // ストリームの長さ(zlib ならヘッダ+deflate+adler)
	level   int
	header  []byte
	plain   []byte
}

// TryUnwrapZip は ZIP コンテナを「スケルトン+展開データ列」に分解する。
// 返り値は (チャンク化する内容 = skeleton||plains, レシピ, 成功か)。
func TryUnwrapZip(orig []byte, maxPlain int64) ([]byte, *ContainerRecipe, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if len(orig) < minStreamSize || !IsZip(orig) {
		return nil, nil, false
	}
	zr, err := zip.NewReader(bytes.NewReader(orig), int64(len(orig)))
	if err != nil || len(zr.File) == 0 || len(zr.File) > maxMembers {
		return nil, nil, false
	}
	var segs []rawSegment
	var totalPlain int64
	for _, f := range zr.File {
		if f.Method != zip.Deflate || f.CompressedSize64 == 0 {
			continue
		}
		off, err := f.DataOffset()
		if err != nil {
			continue
		}
		csize := int64(f.CompressedSize64)
		if off < 0 || off+csize > int64(len(orig)) {
			continue
		}
		stream := orig[off : off+csize]
		fr := flate.NewReader(bytes.NewReader(stream))
		plain, err := io.ReadAll(io.LimitReader(fr, maxPlain-totalPlain+1))
		fr.Close()
		if err != nil || totalPlain+int64(len(plain)) > maxPlain {
			continue // このメンバーは諦める(他のメンバーは拾えるかもしれない)
		}
		level, ok := findLevel(plain, stream)
		if !ok {
			continue // zlib 産ではない → スケルトンに残す
		}
		segs = append(segs, rawSegment{origOff: off, origLen: csize, level: level, plain: plain})
		totalPlain += int64(len(plain))
	}
	return finishContainer(orig, segs)
}

// TryUnwrapPDF は PDF の FlateDecode(zlib)ストリームを分解する。
// PDF のオブジェクト構文は解析せず、"stream" トークン直後の zlib 候補を
// 消費バイト追跡つき inflate + adler32 検証で特定する(誤検出はビット一致
// 検証で排除される)。
func TryUnwrapPDF(orig []byte, maxPlain int64) ([]byte, *ContainerRecipe, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if len(orig) < minStreamSize || !IsPDF(orig) {
		return nil, nil, false
	}
	token := []byte("stream")
	var segs []rawSegment
	var totalPlain int64 // 採用したストリームの伸長合計(出力サイズの上限)
	var inflated int64   // 走査中に伸長した総量(失敗候補も含む。CPU/メモリ増幅の上限)
	pos := 0
	for len(segs) < maxMembers {
		i := bytes.Index(orig[pos:], token)
		if i < 0 {
			break
		}
		dataStart := pos + i + len(token)
		pos = pos + i + 1 // 次の探索位置(候補が不成立でも前進する)
		// PDF 仕様: "stream" の直後は CRLF または LF。
		if dataStart < len(orig) && orig[dataStart] == '\r' {
			dataStart++
		}
		if dataStart >= len(orig) || orig[dataStart] != '\n' {
			continue
		}
		dataStart++
		if dataStart+6 > len(orig) || !IsZlib(orig[dataStart:]) {
			continue
		}
		// 2バイトヘッダ + deflate(消費バイト追跡)+ adler32(4バイト)
		br := bytes.NewReader(orig[dataStart+2:])
		fr := flate.NewReader(br)
		// 伸長予算は「走査全体で伸長した総量(inflated)」で絞る。失敗候補も
		// 計上することで、adler 不一致等で continue する伸長爆弾を大量に並べても
		// 総伸長量が maxPlain 程度で頭打ちになる。旧実装は成功時のみ totalPlain を
		// 加算していたため、失敗候補が毎回 maxPlain まで伸長でき、~1000倍の
		// 増幅型 CPU/メモリ DoS を許していた。
		plain, err := io.ReadAll(io.LimitReader(fr, maxPlain-inflated+1))
		fr.Close()
		inflated += int64(len(plain))
		if inflated > maxPlain {
			break // 走査全体の伸長予算を使い切った(増幅 DoS 防御)
		}
		if err != nil || totalPlain+int64(len(plain)) > maxPlain {
			continue
		}
		consumed := len(orig) - dataStart - 2 - br.Len()
		trailerPos := dataStart + 2 + consumed
		if trailerPos+4 > len(orig) {
			continue
		}
		if adler32.Checksum(plain) != binary.BigEndian.Uint32(orig[trailerPos:trailerPos+4]) {
			continue
		}
		stream := orig[dataStart+2 : trailerPos]
		level, ok := findLevel(plain, stream)
		if !ok {
			continue
		}
		segs = append(segs, rawSegment{
			origOff: int64(dataStart),
			origLen: int64(2 + consumed + 4),
			level:   level,
			header:  append([]byte(nil), orig[dataStart:dataStart+2]...),
			plain:   plain,
		})
		totalPlain += int64(len(plain))
		pos = trailerPos + 4 // 成立したストリームの先まで飛ぶ
	}
	return finishContainer(orig, segs)
}

// finishContainer はセグメント列からスケルトンとレシピを組み立て、
// ビット一致の自己検証をして返す。
func finishContainer(orig []byte, segs []rawSegment) ([]byte, *ContainerRecipe, bool) {
	if len(segs) == 0 {
		return nil, nil, false
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].origOff < segs[j].origOff })
	// 重なりがあれば分解しない(壊れた・悪意ある入力への防御)
	for i := 1; i < len(segs); i++ {
		if segs[i].origOff < segs[i-1].origOff+segs[i-1].origLen {
			return nil, nil, false
		}
	}
	recipe := &ContainerRecipe{}
	var skel, plains []byte
	prev := int64(0)
	for _, sg := range segs {
		skel = append(skel, orig[prev:sg.origOff]...)
		recipe.Segments = append(recipe.Segments, ContainerSegment{
			SkelPos:  int64(len(skel)),
			PlainLen: int64(len(sg.plain)),
			Level:    sg.level,
			Header:   sg.header,
		})
		plains = append(plains, sg.plain...)
		prev = sg.origOff + sg.origLen
	}
	skel = append(skel, orig[prev:]...)
	recipe.SkelLen = int64(len(skel))
	chunked := append(skel, plains...)

	// 自己検証: レシピから元をビット一致で再現できる場合のみ採用する。
	rebuilt, err := ReconstructContainer(recipe, chunked)
	if err != nil || !bytes.Equal(rebuilt, orig) {
		return nil, nil, false
	}
	return chunked, recipe, true
}

// ReconstructContainer はレシピとチャンク化内容(skeleton||plains)から
// 元のコンテナをビット単位で再構成する。
func ReconstructContainer(recipe *ContainerRecipe, chunked []byte) ([]byte, error) {
	if recipe.SkelLen < 0 || recipe.SkelLen > int64(len(chunked)) {
		return nil, fmt.Errorf("レシピのスケルトン長が不正です")
	}
	skel := chunked[:recipe.SkelLen]
	plains := chunked[recipe.SkelLen:]
	var out []byte
	skelPrev, plainOff := int64(0), int64(0)
	for _, sg := range recipe.Segments {
		if sg.SkelPos < skelPrev || sg.SkelPos > int64(len(skel)) ||
			sg.PlainLen < 0 || plainOff+sg.PlainLen > int64(len(plains)) {
			return nil, fmt.Errorf("レシピのセグメントが不正です")
		}
		out = append(out, skel[skelPrev:sg.SkelPos]...)
		plain := plains[plainOff : plainOff+sg.PlainLen]
		if len(sg.Header) > 0 {
			stream, err := ReconstructZlib(sg.Header, sg.Level, plain)
			if err != nil {
				return nil, err
			}
			out = append(out, stream...)
		} else {
			stream, err := deflateExact(plain, sg.Level)
			if err != nil {
				return nil, err
			}
			out = append(out, stream...)
		}
		skelPrev = sg.SkelPos
		plainOff += sg.PlainLen
	}
	if plainOff != int64(len(plains)) {
		return nil, fmt.Errorf("展開データに余りがあります")
	}
	return append(out, skel[skelPrev:]...), nil
}
