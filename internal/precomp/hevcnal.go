package precomp

// HEVC(H.265)の NAL 層。Annex B のスタートコード・エミュレーション防止は
// H.264 と同一なので splitAnnexB / unescapeRBSP / escapeRBSP を共用する。
//
// NAL ヘッダは2バイト:
//   forbidden_zero_bit(1) nal_unit_type(6) nuh_layer_id(6) nuh_temporal_id_plus1(3)

// HEVC NAL タイプ(抜粋)。
const (
	hevcNALTrailN   = 0
	hevcNALTrailR   = 1
	hevcNALIDRWRadl = 19
	hevcNALIDRNLP   = 20
	hevcNALCRA      = 21
	hevcNALVPS      = 32
	hevcNALSPS      = 33
	hevcNALPPS      = 34
	hevcNALAUD      = 35
	hevcNALEOS      = 36
	hevcNALEOB      = 37
	hevcNALFD       = 38
	hevcNALSEIPre   = 39
	hevcNALSEISuf   = 40
)

// hevcNALType は NAL ヘッダからタイプを取り出す。
func hevcNALType(b []byte) int {
	if len(b) < 2 {
		return -1
	}
	return int(b[0]>>1) & 0x3F
}

// hevcIsIRAP は IRAP(IDR/CRA/BLA)か。
func hevcIsIRAP(t int) bool { return t >= 16 && t <= 23 }

// hevcIsSlice はスライスセグメント NAL か(VCL)。
func hevcIsSlice(t int) bool { return t >= 0 && t <= 31 }

// IsHEVC は Annex B の HEVC エレメンタリストリームらしいかを判定する。
func IsHEVC(head []byte) bool {
	sl := startCodeLen(head)
	if sl == 0 || len(head) < sl+2 {
		return false
	}
	b := head[sl]
	if b>>7 != 0 { // forbidden_zero_bit
		return false
	}
	switch hevcNALType(head[sl:]) {
	case hevcNALVPS, hevcNALSPS, hevcNALAUD, hevcNALSEIPre:
		return true
	}
	return false
}
