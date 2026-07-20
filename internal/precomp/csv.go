package precomp

// CSV / 区切りテキストの可逆な列指向(columnar)変換。
//
// 行指向の CSV は「1行 = 異種フィールドの並び」なので、圧縮器から見ると
// 型も分布も違う値が交互に来る。列指向に転置すると各列が同種の値
// (タイムスタンプ・列挙・数値)だけになり、LZ/エントロピー符号が桁違いに
// 効く(列ストア=Parquet が小さいのと同じ原理)。実測(疑似アクセスログ
// CSV)で行指向比 **-21.8%**、数値列を差分(delta)符号化するとさらに
// **-25.2%**(RESEARCH.md §4.30)。
//
// 可逆性: 転置は「'\n' で行、',' で列に split → 列順に再連結」で、Go の
// split/join は完全な逆変換。矩形(全行が同じ列数)でありさえすれば、
// 引用符・CRLF の '\r'・非印字バイトも**フィールド内容としてそのまま往復**
// する(RFC4180 の引用解釈はしない=解釈しないから壊れない)。数値列の
// delta は「その列の各値が int64 に正準往復する時だけ」適用する。最後に
// 復元して orig とバイト一致を検証してからのみ採用し、読み出し時も
// SHA-256 で最終検証する。純Go・cgo 不要。

import (
	"bytes"
	"errors"
	"strconv"
)

// csvMinRows / csvMinCols は変換を試す最小規模(小さすぎると利得が出ない)。
const (
	csvMinRows = 8
	csvMinCols = 2
	csvMaxCols = 4096 // 病的に広い行を弾く(メモリ保護)
)

// CSVRecipe は列指向変換の再構成レシピ。
type CSVRecipe struct {
	Cols       int     `json:"c"`            // 列数
	Rows       int     `json:"r"`            // 行数(ヘッダ含む全行)
	TrailingNL bool    `json:"t,omitempty"`  // 元が改行で終わるか
	CRLF       bool    `json:"crlf,omitempty"` // 全行が '\r\n' 終端(復元時に '\r' を戻す)
	Delta      []bool  `json:"d,omitempty"`  // 旧: 列ごとの数値 delta フラグ(後方互換)
	Codec      []uint8 `json:"cc,omitempty"` // 新: 列ごとのコーデック(raw/delta/dict)
	ColBytes   []int   `json:"cb,omitempty"` // 新: 列ごとのセグメントバイト長
}

// CSVUnwrapped は分解結果。
type CSVUnwrapped struct {
	Chunked []byte
	Recipe  *CSVRecipe
}

// IsCSV は head が列区切りテキストらしいかの安価な事前判定。矩形性の本判定は
// TryUnwrapCSV が全体を走査して行う(ここは全体バッファ確保の門番)。
func IsCSV(head []byte) bool {
	if len(head) < 4 {
		return false
	}
	// 先頭行(最初の '\n' まで)を見て、印字可能で ',' を含むこと。
	line := head
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		line = head[:i]
	}
	if len(line) < 3 || bytes.IndexByte(line, ',') < 0 {
		return false
	}
	for _, b := range line {
		// 印字可能 ASCII とタブのみ(先頭行にヌル・制御が混じるなら非CSV)。
		if b != '\t' && (b < 0x20 || b > 0x7E) {
			return false
		}
	}
	return true
}

// parseCanonInt は b が int64 に正準往復する(parse→format が元と一致)
// なら値と true を返す。先頭ゼロ・'+'・"-0" 等は正準でないので false。
//
// 桁数上限は 19(ナノ秒 Unix タイムスタンプ=19桁が delta 対象になる)。
// 20 桁以上は int64 に必ず収まらないので弾く。19 桁でも int64 範囲外は
// ParseInt がエラーにし、FormatInt==元 の正準検査も通らないので安全。
func parseCanonInt(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 19 {
		return 0, false
	}
	v, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, false
	}
	if strconv.FormatInt(v, 10) != string(b) {
		return 0, false
	}
	return v, true
}

// parseCanonIntLegacy は桁上限を 19 へ緩める(§4.45)より前の parseCanonInt
// (18桁上限)。**旧形式レシピ(decodeLegacyGrid)の delta 基準 row0 は、当時の
// 規則で計算しないと既存保存物の復元が食い違う**:旧エンコーダは 19桁 row0 を
// parseCanonInt(18桁上限)で弾いて基準 0 として delta を格納した。新 parseCanonInt
// で復号すると基準が V0 になり全データ行が V0 ぶんずれる(読み出し SHA-256 で
// 検出され、読めていた物が読めなくなる=データ損失)。よって旧経路は当時の
// 18桁上限で基準を再現する。新経路の基準導出は下記 deltaBaseCanon に凍結した。
func parseCanonIntLegacy(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 18 {
		return 0, false
	}
	v, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, false
	}
	if strconv.FormatInt(v, 10) != string(b) {
		return 0, false
	}
	return v, true
}

// deltaBaseCanon は新形式(ColBytes)colDelta の「delta 基準値」を row0 から導出する
// 凍結境界。基準が無い(row0 が正準 int でない)なら 0 を返す。
//
// **この関数の意味論は絶対に変えてはならない。** 基準は encode と decode の
// 両方で使われ、しかも保存済みレシピは「保存時の規則」で復元されねばならない。
// parseCanonInt は列の delta 適格性判定(データ行が正準 int か)にも使われており、
// 将来その桁上限等を性能・網羅目的で変える動機がありうる。かつて parseCanonInt を
// 18→19桁へ広げた際、それが基準導出に波及して旧 delta レシピを壊した(データ損失)。
// 適格性判定(parseCanonInt)と基準導出(本関数)を分離し、後者を現行 19桁で凍結する
// ことで、parseCanonInt が今後変わっても保存物の復元は不変に保たれる。桁規則を
// 変えたい場合は本関数を編集せず、新しいコーデックバージョンを追加すること。
func deltaBaseCanon(b []byte) int64 {
	if len(b) == 0 || len(b) > 19 {
		return 0
	}
	v, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0
	}
	if strconv.FormatInt(v, 10) != string(b) {
		return 0
	}
	return v
}

// TryUnwrapCSV は矩形 CSV を列指向(+数値列 delta)に変換する。
func TryUnwrapCSV(orig []byte, maxPlain int64) (*CSVUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 32 {
		return nil, false
	}
	// 行に分割。末尾改行を検出して除く。
	lines := bytes.Split(orig, []byte{'\n'})
	trailingNL := false
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
		trailingNL = true
	}
	if len(lines) < csvMinRows {
		return nil, false
	}
	// CRLF 検出: 全行が '\r' 終端なら、末尾 '\r' を行区切りの一部とみなして
	// 剥がす(最後の列に '\r' が閉じ込められて delta 化を妨げるのを解消)。
	// 復元時に各行へ '\r' を戻す。剥がし+戻しは厳密な逆変換で、往復検証が保証。
	crlf := true
	for _, ln := range lines {
		if len(ln) == 0 || ln[len(ln)-1] != '\r' {
			crlf = false
			break
		}
	}
	if crlf {
		for i := range lines {
			lines[i] = lines[i][:len(lines[i])-1]
		}
	}
	ncol := bytes.Count(lines[0], []byte{','}) + 1
	if ncol < csvMinCols || ncol > csvMaxCols {
		return nil, false
	}
	nrows := len(lines)
	// 各行を列に分割し、矩形性を確認。
	grid := make([][][]byte, nrows)
	for i, ln := range lines {
		f := bytes.Split(ln, []byte{','})
		if len(f) != ncol {
			return nil, false // 非矩形 → 変換不可
		}
		grid[i] = f
	}

	// 列ごとに最適コーデック(raw/delta/dict)を選び、連結ブロブを作る。
	chosen, codecs, colBytes := encodeColumns(grid, ncol, nrows)

	// 行指向(原文)より確実に縮む時だけ採用(probe 実測の best-of)。
	if probeLen(chosen) >= probeLen(orig) {
		return nil, false
	}

	recipe := &CSVRecipe{
		Cols: ncol, Rows: nrows, TrailingNL: trailingNL, CRLF: crlf,
		Codec: codecs, ColBytes: colBytes,
	}

	// 最終安全弁: 復元して orig とバイト一致を確認。
	if rt, err := ReconstructCSV(recipe, chosen); err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &CSVUnwrapped{Chunked: chosen, Recipe: recipe}, true
}

// probeLen は precomp 共通の probe エンコーダで圧縮したバイト数。
func probeLen(b []byte) int {
	return len(jpegProbeEncoder.EncodeAll(b, make([]byte, 0, len(b)/2)))
}

// ReconstructCSV はレシピと列指向ブロブから元の CSV をバイト単位で戻す。
func ReconstructCSV(recipe *CSVRecipe, blob []byte) ([]byte, error) {
	if recipe == nil || recipe.Cols < 1 || recipe.Rows < 1 {
		return nil, errors.New("CSV レシピが不正です")
	}
	ncol, nrows := recipe.Cols, recipe.Rows
	var fields [][][]byte
	if recipe.ColBytes != nil {
		// 新形式: 列ごとのコーデック+セグメント長で復元。
		g, err := decodeColumns(blob, recipe.Codec, recipe.ColBytes, ncol, nrows)
		if err != nil {
			return nil, err
		}
		fields = g
	} else {
		// 旧形式(後方互換): 固定行グリッド + 一括 delta(共有ヘルパ)。
		g, err := decodeLegacyGrid(blob, recipe.Delta, ncol, nrows)
		if err != nil {
			return nil, err
		}
		fields = g
	}
	// 行を ',' で、行同士を '\n' で連結。
	var out bytes.Buffer
	out.Grow(len(blob))
	for r := 0; r < nrows; r++ {
		if r > 0 {
			out.WriteByte('\n')
		}
		for c := 0; c < ncol; c++ {
			if c > 0 {
				out.WriteByte(',')
			}
			out.Write(fields[r][c])
		}
		if recipe.CRLF { // 各行末に '\r' を戻す('\r\n' 終端)
			out.WriteByte('\r')
		}
	}
	if recipe.TrailingNL {
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}
