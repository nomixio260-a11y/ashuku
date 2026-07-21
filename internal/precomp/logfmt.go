package precomp

// logfmt(key=value 区切りログ)の可逆な列指向変換。
//
// Go サービス・Heroku・Docker・多くのクラウドログの主流形式 "ts=... level=info
// msg=\"...\" latency=12ms" は、キーが値に貼り付いたままなので、空白区切りログ経路
// (logline.go)では各フィールドが "key=value" のまま列にならず、しかも msg の
// 引用有無で毎行フィールド数が変わって矩形にならない。ここでは **"key=" を骨格へ、
// 値だけを列へ**分離する。値は「裸トークン(次の空白まで)」または「引用文字列
// (エスケープ透過)」で、引用符は値側に含める(引用の有無が混在しても骨格は同一)。
// これで ts が正準 int(delta)、trace_id が hex(hexpack)、enum が dict になり縮む。
// 全行の骨格が一致するときだけ対象。採用前に復元してバイト一致検証。純Go。

import (
	"bytes"
	"errors"
)

const (
	logfmtMinRows = 6
	logfmtMinCols = 2
	logfmtMaxCols = 4096
)

// LogfmtRecipe は logfmt 列指向変換の再構成レシピ。
type LogfmtRecipe struct {
	Skeleton   []byte  `json:"sk"`
	Cols       int     `json:"c"`
	Rows       int     `json:"r"`
	TrailingNL bool    `json:"t,omitempty"`
	Codec      []uint8 `json:"cc,omitempty"`
	ColBytes   []int   `json:"cb,omitempty"`
}

// LogfmtUnwrapped は分解結果。
type LogfmtUnwrapped struct {
	Chunked []byte
	Recipe  *LogfmtRecipe
}

// IsLogfmt は head が logfmt らしいか(先頭行に 2 つ以上の key=value)を安価に判定。
// 矩形性の本判定は TryUnwrapLogfmt が全行走査で行う。
func IsLogfmt(head []byte) bool {
	if len(head) < 16 {
		return false
	}
	line := head
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		line = head[:i]
	}
	if len(line) < 16 || line[0] == '{' || line[0] == '[' {
		return false
	}
	for _, b := range line {
		if b == 0x00 || b == '\r' {
			return false
		}
		if b < 0x20 && b != '\t' {
			return false
		}
	}
	// "識別子=" のペアが 2 つ以上。'=' の直前が空白/'=' でないこと。
	pairs := 0
	prevSpace := true
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '=' && !prevSpace && i > 0 && line[i-1] != '=' {
			pairs++
		}
		prevSpace = c == ' '
	}
	return pairs >= 2
}

// logfmtSkeletonize は 1 行を "key=" 骨格と値フィールド列に分ける。骨格+値の連結は
// 構成上つねに元の行に一致する。key=value 形式でない行は ok=false。
func logfmtSkeletonize(line []byte) (skel []byte, fields [][]byte, ok bool) {
	n := len(line)
	if n == 0 || bytes.IndexByte(line, 0x00) >= 0 {
		return nil, nil, false
	}
	var sk bytes.Buffer
	sk.Grow(n)
	i := 0
	for i < n {
		// 区切り/先頭の空白を骨格へ。
		for i < n && line[i] == ' ' {
			sk.WriteByte(' ')
			i++
		}
		if i >= n {
			break
		}
		// キー: '=' か空白まで。
		keyStart := i
		for i < n && line[i] != '=' && line[i] != ' ' {
			i++
		}
		if i >= n || line[i] != '=' || i == keyStart {
			return nil, nil, false // 裸トークン/空キー → logfmt でない
		}
		sk.Write(line[keyStart:i])
		sk.WriteByte('=')
		i++ // '='
		// 値: 引用文字列 or 裸トークン。引用符は値側に含める。
		valStart := i
		if i < n && line[i] == '"' {
			i++ // 開き "
			for i < n && line[i] != '"' {
				if line[i] == '\\' && i+1 < n {
					i++ // エスケープ透過
				}
				i++
			}
			if i >= n {
				return nil, nil, false // 閉じ " なし
			}
			i++ // 閉じ "
		} else {
			for i < n && line[i] != ' ' {
				i++
			}
		}
		fields = append(fields, line[valStart:i])
		sk.WriteByte(0x00)
	}
	if len(fields) == 0 {
		return nil, nil, false
	}
	return sk.Bytes(), fields, true
}

// TryUnwrapLogfmt は logfmt ログを列指向に変換する。
func TryUnwrapLogfmt(orig []byte, maxPlain int64) (*LogfmtUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 64 {
		return nil, false
	}
	lines := bytes.Split(orig, []byte{'\n'})
	trailingNL := false
	if m := len(lines); m > 0 && len(lines[m-1]) == 0 {
		lines = lines[:m-1]
		trailingNL = true
	}
	if len(lines) < logfmtMinRows {
		return nil, false
	}
	skel0, f0, ok := logfmtSkeletonize(lines[0])
	if !ok {
		return nil, false
	}
	ncol := len(f0)
	if ncol < logfmtMinCols || ncol > logfmtMaxCols {
		return nil, false
	}
	nrows := len(lines)
	grid := make([][][]byte, nrows)
	grid[0] = f0
	for r := 1; r < nrows; r++ {
		sk, fs, ok := logfmtSkeletonize(lines[r])
		if !ok || len(fs) != ncol || !bytes.Equal(sk, skel0) {
			return nil, false // 骨格不一致 → 素通し
		}
		grid[r] = fs
	}

	chosen, codecs, colBytes := encodeColumns(grid, ncol, nrows)
	if probeLen(chosen) >= probeLen(orig) {
		return nil, false
	}
	recipe := &LogfmtRecipe{
		Skeleton: append([]byte(nil), skel0...), Cols: ncol, Rows: nrows,
		TrailingNL: trailingNL, Codec: codecs, ColBytes: colBytes,
	}
	if rt, err := ReconstructLogfmt(recipe, chosen); err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &LogfmtUnwrapped{Chunked: chosen, Recipe: recipe}, true
}

// ReconstructLogfmt はレシピと列指向ブロブから元の logfmt ログをバイト単位で戻す。
func ReconstructLogfmt(recipe *LogfmtRecipe, blob []byte) ([]byte, error) {
	if recipe == nil || recipe.Cols < 1 || recipe.Rows < 1 {
		return nil, errors.New("Logfmt レシピが不正です")
	}
	ncol, nrows := recipe.Cols, recipe.Rows
	segs := bytes.Split(recipe.Skeleton, []byte{0x00})
	if len(segs) != ncol+1 {
		return nil, errors.New("Logfmt 骨格のプレースホルダ数が不一致")
	}
	if recipe.ColBytes == nil {
		return nil, errors.New("Logfmt レシピに列情報がない")
	}
	vals, err := decodeColumns(blob, recipe.Codec, recipe.ColBytes, ncol, nrows)
	if err != nil {
		return nil, err
	}
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
