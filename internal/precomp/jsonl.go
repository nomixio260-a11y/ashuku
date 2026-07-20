package precomp

// JSONL(改行区切り JSON、= 構造化ログの定番)の可逆な列指向変換。
//
// CSV(§4.30)と同じ「構造を明かして転置する」発想を JSON レコードに広げる。
// 同一スキーマの JSON オブジェクトが1行ずつ並ぶ JSONL は、各行が
// 「同じ骨格(キー名・区切り・空白)+ 異なる値」でできている。そこで
// 1行を **骨格(skeleton)** と **値(value)の並び** に分解する。骨格が
// 全行で同一なら、骨格を1つだけ保存し、値を列指向に転置する。数値列は
// CSV と同じく delta 符号化。実測(疑似 JSON アクセスログ)で行指向比
// **-26%**(RESEARCH.md §4.31)。
//
// 可逆性の核心: 骨格は「その行から値の範囲を 0x00 に置換したもの」。JSON の
// 1行に生の 0x00/0x0A は現れない(現れるなら \u0000 / \n エスケープ)ため、
// 「骨格を 0x00 で split して値と交互に連結」で**必ず元の行に戻る**
// (値の中身を意味解釈しない=構造だけ触るから壊れない)。骨格が同一かつ
// 値数が同じ行だけを対象にし、非対象は素通し。採用前に復元して orig と
// バイト一致を検証し、読み出し時も SHA-256。純Go・cgo 不要。

import (
	"bytes"
	"errors"
)

const (
	jsonlMinRows = 8
	jsonlMinCols = 2
	jsonlMaxCols = 4096
)

// JSONLRecipe は JSONL 列指向変換の再構成レシピ。
type JSONLRecipe struct {
	Skeleton   []byte  `json:"sk"`           // 値を 0x00 に置換した行の骨格(全行共通)
	Cols       int     `json:"c"`            // 値(列)数
	Rows       int     `json:"r"`            // 行数
	TrailingNL bool    `json:"t,omitempty"`  // 元が改行で終わるか
	Delta      []bool  `json:"d,omitempty"`  // 旧: 列ごとの数値 delta フラグ(後方互換)
	Codec      []uint8 `json:"cc,omitempty"` // 新: 列ごとのコーデック(raw/delta/dict)
	ColBytes   []int   `json:"cb,omitempty"` // 新: 列ごとのセグメントバイト長
}

// JSONLUnwrapped は分解結果。
type JSONLUnwrapped struct {
	Chunked []byte
	Recipe  *JSONLRecipe
}

// IsJSONL は head が JSONL(先頭行が JSON オブジェクト)らしいかの安価な判定。
func IsJSONL(head []byte) bool {
	i := 0
	for i < len(head) && (head[i] == ' ' || head[i] == '\t') {
		i++
	}
	if i >= len(head) || head[i] != '{' {
		return false
	}
	// 先頭行に "..." : が現れること(オブジェクトらしさ)。
	line := head[i:]
	if j := bytes.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	return bytes.IndexByte(line, '"') >= 0 && bytes.IndexByte(line, ':') >= 0
}

// skeletonizeFlat は平坦な JSON オブジェクト行を解析し、値のバイト範囲と
// 骨格(値を 0x00 に置換した行)を返す。入れ子・配列値・想定外の構文は
// ok=false(=素通し)。骨格+値の連結は構成上つねに元の行に一致する。
func skeletonizeFlat(line []byte) (skel []byte, vals [][]byte, ok bool) {
	n := len(line)
	if n < 2 || line[0] != '{' {
		return nil, nil, false
	}
	var sk bytes.Buffer
	sk.Grow(n)
	i := 0
	emit := func() { sk.WriteByte(line[i]); i++ }
	ws := func() {
		for i < n && (line[i] == ' ' || line[i] == '\t') {
			emit()
		}
	}
	emit() // '{'
	ws()
	if i < n && line[i] == '}' {
		emit()
		if i != n {
			return nil, nil, false
		}
		return sk.Bytes(), nil, false // 空オブジェクト=利得なし
	}
	for {
		ws()
		// キー文字列
		if i >= n || line[i] != '"' {
			return nil, nil, false
		}
		emit()
		for i < n && line[i] != '"' {
			if line[i] == '\\' {
				emit()
				if i >= n {
					return nil, nil, false
				}
			}
			emit()
		}
		if i >= n {
			return nil, nil, false
		}
		emit() // 閉じ "
		ws()
		if i >= n || line[i] != ':' {
			return nil, nil, false
		}
		emit() // ':'
		ws()
		// 値
		if i >= n {
			return nil, nil, false
		}
		vstart := i
		switch line[i] {
		case '"':
			i++
			for i < n && line[i] != '"' {
				if line[i] == '\\' {
					i++
					if i >= n {
						return nil, nil, false
					}
				}
				i++
			}
			if i >= n {
				return nil, nil, false
			}
			i++ // 閉じ "
		case '{', '[':
			return nil, nil, false // 入れ子は対象外
		default: // 数値・true・false・null
			for i < n && line[i] != ',' && line[i] != '}' && line[i] != ' ' && line[i] != '\t' {
				i++
			}
			if i == vstart {
				return nil, nil, false
			}
		}
		vals = append(vals, line[vstart:i])
		sk.WriteByte(0x00) // 値プレースホルダ
		ws()
		if i < n && line[i] == ',' {
			emit()
			continue
		}
		if i < n && line[i] == '}' {
			emit()
			break
		}
		return nil, nil, false
	}
	if i != n {
		return nil, nil, false // 末尾に余分
	}
	return sk.Bytes(), vals, true
}

// TryUnwrapJSONL は同一スキーマの JSONL を列指向(+数値列 delta)に変換する。
func TryUnwrapJSONL(orig []byte, maxPlain int64) (*JSONLUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 32 {
		return nil, false
	}
	lines := bytes.Split(orig, []byte{'\n'})
	trailingNL := false
	if m := len(lines); m > 0 && len(lines[m-1]) == 0 {
		lines = lines[:m-1]
		trailingNL = true
	}
	if len(lines) < jsonlMinRows {
		return nil, false
	}
	skel0, vals0, ok := skeletonizeFlat(lines[0])
	if !ok {
		return nil, false
	}
	ncol := len(vals0)
	if ncol < jsonlMinCols || ncol > jsonlMaxCols {
		return nil, false
	}
	nrows := len(lines)
	grid := make([][][]byte, nrows)
	grid[0] = vals0
	for r := 1; r < nrows; r++ {
		sk, vs, ok := skeletonizeFlat(lines[r])
		if !ok || len(vs) != ncol || !bytes.Equal(sk, skel0) {
			return nil, false // スキーマ不一致 → 素通し
		}
		grid[r] = vs
	}

	// 列ごとに最適コーデック(raw/delta/dict)を選び、連結ブロブを作る。
	chosen, codecs, colBytes := encodeColumns(grid, ncol, nrows)
	if probeLen(chosen) >= probeLen(orig) {
		return nil, false
	}

	recipe := &JSONLRecipe{
		Skeleton: append([]byte(nil), skel0...), Cols: ncol, Rows: nrows,
		TrailingNL: trailingNL, Codec: codecs, ColBytes: colBytes,
	}
	if rt, err := ReconstructJSONL(recipe, chosen); err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &JSONLUnwrapped{Chunked: chosen, Recipe: recipe}, true
}

// ReconstructJSONL はレシピと列指向ブロブから元の JSONL をバイト単位で戻す。
func ReconstructJSONL(recipe *JSONLRecipe, blob []byte) ([]byte, error) {
	if recipe == nil || recipe.Cols < 1 || recipe.Rows < 1 {
		return nil, errors.New("JSONL レシピが不正です")
	}
	ncol, nrows := recipe.Cols, recipe.Rows
	// 骨格を 0x00 で分割 → ncol+1 セグメント(値 ncol 個と交互)。
	segs := bytes.Split(recipe.Skeleton, []byte{0x00})
	if len(segs) != ncol+1 {
		return nil, errors.New("JSONL 骨格のプレースホルダ数が不一致")
	}
	var vals [][][]byte
	if recipe.ColBytes != nil {
		// 新形式: 列ごとのコーデック+セグメント長で復元。
		g, err := decodeColumns(blob, recipe.Codec, recipe.ColBytes, ncol, nrows)
		if err != nil {
			return nil, err
		}
		vals = g
	} else {
		// 旧形式(後方互換): 固定行グリッド + 一括 delta(共有ヘルパ)。
		g, err := decodeLegacyGrid(blob, recipe.Delta, ncol, nrows)
		if err != nil {
			return nil, err
		}
		vals = g
	}
	// 各行 = seg[0] v0 seg[1] v1 ... seg[ncol-1] v(ncol-1) seg[ncol]。
	var out bytes.Buffer
	out.Grow(len(blob) + nrows*len(recipe.Skeleton)/max1(ncol))
	for r := 0; r < nrows; r++ {
		if r > 0 {
			out.WriteByte('\n')
		}
		for c := 0; c < ncol; c++ {
			out.Write(segs[c])
			out.Write(vals[r][c])
		}
		out.Write(segs[ncol])
	}
	if recipe.TrailingNL {
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
