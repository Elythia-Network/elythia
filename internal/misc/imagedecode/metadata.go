package imagedecode

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrMalformedMetadata reports embedded EXIF or ICC data that would make the
// metadata parsers used by imaging allocate far more than the input holds.
var ErrMalformedMetadata = errors.New("imagedecode: malformed embedded metadata")

// maxICCProfileBytes caps a decompressed PNG iCCP profile. 実在のプロファイルは
// 大きいものでも数百 KB (印刷用の CMYK で 500KB 程度)。
const maxICCProfileBytes = 4 << 20

// checkEmbeddedMetadata validates the EXIF and ICC data embedded in a JPEG,
// PNG, WebP or TIFF file before imaging parses it.
//
// **imaging のメタデータ処理は宣言値をそのまま確保に使う (実測)。** EXIF は
// rwcarlsen/goexif が「要素数 × 型の大きさ」を uint32 で計算して桁あふれし、
// 要素数ぶんの slice を確保する (88 バイトの WebP で 8GiB、24GiB で fatal)。
// ICC は imaging の ProfileReader がタグ表の件数とタグの終端、`curv` の要素数を
// 検査せずに確保する。PNG は chunk の長さのまま読み、iCCP の zlib を上限無しに
// 展開する。media proxy は未認証で任意の画像をここへ通すので、どれも 1KB 未満の
// 画像でプロセスを落とせた。Go の大確保の失敗は recover できない。
//
// ここでは各形式から EXIF / ICC を自前で (長さを検査しながら) 取り出し、
// パーサが確保する量が入力の長さを超えないことを確かめる。
func checkEmbeddedMetadata(data []byte) error {
	switch {
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8:
		return checkJPEGMetadata(data)
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return checkPNGMetadata(data)
	case isWebP(data):
		return checkWebPEmbeddedMetadata(data)
	case len(data) >= 4 && (string(data[:4]) == "II*\x00" || string(data[:4]) == "MM\x00*"):
		return checkTIFFStructure(data)
	}
	return nil
}

func checkJPEGMetadata(data []byte) error {
	var icc [][]byte
	pos := 2
	for pos+4 <= len(data) {
		if data[pos] != 0xFF {
			return nil // 以降はエントロピー符号化データ。imaging も読まない
		}
		marker := data[pos+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 || marker == 0xFF {
			pos += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			break
		}
		n := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		if n < 2 || pos+2+n > len(data) {
			break // imaging も読めずに止まる
		}
		seg := data[pos+4 : pos+2+n]
		switch {
		case marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")):
			if err := checkTIFFStructure(seg[6:]); err != nil {
				return err
			}
		case marker == 0xE2 && bytes.HasPrefix(seg, []byte("ICC_PROFILE\x00")) && len(seg) >= 14:
			icc = append(icc, seg[14:])
		}
		pos += 2 + n
	}
	if len(icc) > 0 {
		return checkICCProfile(bytes.Join(icc, nil))
	}
	return nil
}

func checkPNGMetadata(data []byte) error {
	pos := 8
	for pos+8 <= len(data) {
		n := uint64(binary.BigEndian.Uint32(data[pos : pos+4]))
		typ := string(data[pos+4 : pos+8])
		// imaging は長さ + 4 (CRC) をそのまま確保して読む。uint32 で足すので
		// 0xFFFFFFFC 以上では桁あふれもする
		if n+4 > uint64(len(data)-pos-8) {
			return fmt.Errorf("%w: PNG %s chunk of %d bytes overruns the file", ErrMalformedMetadata, typ, n)
		}
		body := data[pos+8 : pos+8+int(n)]
		switch typ {
		case "eXIf":
			if err := checkTIFFStructure(body); err != nil {
				return err
			}
		case "iCCP":
			profile, err := inflateICCP(body)
			if err != nil {
				return err
			}
			if profile != nil {
				if err := checkICCProfile(profile); err != nil {
					return err
				}
			}
		case "IDAT", "IEND":
			return nil
		}
		pos += 12 + int(n)
	}
	return nil
}

// inflateICCP decompresses an iCCP chunk body with a size cap. nil means the
// chunk is not in a form imaging would decompress.
func inflateICCP(body []byte) ([]byte, error) {
	idx := bytes.IndexByte(body, 0)
	if idx < 0 || idx > 80 || idx+2 > len(body) || body[idx+1] != 0 {
		return nil, nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(body[idx+2:]))
	if err != nil {
		return nil, nil
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(io.LimitReader(zr, maxICCProfileBytes+1))
	if len(out) > maxICCProfileBytes {
		return nil, fmt.Errorf("%w: PNG iCCP expands beyond %d bytes", ErrMalformedMetadata, maxICCProfileBytes)
	}
	if err != nil {
		return nil, nil // imaging も展開に失敗して ICC を捨てる
	}
	return out, nil
}

// checkWebPEmbeddedMetadata validates the EXIF / ICCP payloads of a WebP.
// 長さそのものの検査は checkImagingWebPMetadata が受け持つ。
func checkWebPEmbeddedMetadata(data []byte) error {
	chunks, _, err := webpChunks(data)
	if err != nil {
		return nil // decodeWebP が同じ理由で拒否する
	}
	for _, c := range chunks {
		switch c.typ {
		case "EXIF":
			p := c.payload
			if bytes.HasPrefix(p, []byte("Exif\x00\x00")) {
				p = p[6:]
			}
			if err := checkTIFFStructure(p); err != nil {
				return err
			}
		case "ICCP":
			if err := checkICCProfile(c.payload); err != nil {
				return err
			}
		}
	}
	return nil
}

// tiffTypeSizes mirrors goexif's typeSize table.
var tiffTypeSizes = map[uint16]uint64{
	1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 6: 1, 7: 1, 8: 2, 9: 4, 10: 8, 11: 4, 12: 8,
}

// maxTIFFIFDs bounds the IFD chain walk. goexif は next 連鎖を循環検出なしに
// 辿るので、循環した連鎖では IFD を無限に積む。
const maxTIFFIFDs = 64

// checkTIFFStructure validates a TIFF structure (a TIFF file or the EXIF block)
// the way goexif will read it: every tag's element count times element size,
// computed without overflow, must fit in the data, and the IFD chain must end.
func checkTIFFStructure(data []byte) error {
	if len(data) < 8 {
		return nil // goexif が読めずにエラーにする
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return nil
	}
	size := uint64(len(data))
	seen := map[uint32]bool{}
	var walk func(off uint32, chain bool) error
	walk = func(off uint32, chain bool) error {
		for off != 0 {
			if seen[off] {
				return fmt.Errorf("%w: TIFF IFD loop at %d", ErrMalformedMetadata, off)
			}
			seen[off] = true
			if len(seen) > maxTIFFIFDs {
				return fmt.Errorf("%w: more than %d TIFF IFDs", ErrMalformedMetadata, maxTIFFIFDs)
			}
			if uint64(off)+2 > size {
				return nil
			}
			n := int(int16(order.Uint16(data[off : off+2])))
			base := uint64(off) + 2
			for i := 0; i < n; i++ {
				e := base + uint64(i)*12
				if e+12 > size {
					return nil // goexif は読めずにエラーにする
				}
				tag := order.Uint16(data[e : e+2])
				typ := order.Uint16(data[e+2 : e+4])
				count := uint64(order.Uint32(data[e+4 : e+8]))
				if ts, ok := tiffTypeSizes[typ]; ok && count*ts > size {
					return fmt.Errorf("%w: TIFF tag 0x%04x declares %d values", ErrMalformedMetadata, tag, count)
				}
				switch tag {
				case 0x8769, 0x8825, 0xA005: // Exif / GPS / Interop の sub-IFD
					if err := walk(order.Uint32(data[e+8:e+12]), false); err != nil {
						return err
					}
				}
			}
			if !chain {
				return nil
			}
			next := base + uint64(n)*12
			if n < 0 || next+4 > size {
				return nil
			}
			off = order.Uint32(data[next : next+4])
		}
		return nil
	}
	return walk(order.Uint32(data[4:8]), true)
}

// checkICCProfile validates an ICC profile the way imaging's ProfileReader and
// curve decoder will read it.
func checkICCProfile(p []byte) error {
	if len(p) < 132 {
		return nil // imaging はヘッダを読めずにエラーにする
	}
	size := uint64(len(p))
	count := uint64(binary.BigEndian.Uint32(p[128:132]))
	tableEnd := 132 + count*12
	if tableEnd > size {
		return fmt.Errorf("%w: ICC tag table of %d entries overruns the profile", ErrMalformedMetadata, count)
	}
	for i := uint64(0); i < count; i++ {
		e := 132 + i*12
		off := uint64(binary.BigEndian.Uint32(p[e+4 : e+8]))
		n := uint64(binary.BigEndian.Uint32(p[e+8 : e+12]))
		// imaging はタグの終端を uint32 で足し、タグ表の直後からの差で切り出す。
		// 終端がプロファイルを越える / タグ表の中を指すと、確保が膨らむか
		// slice の範囲外になる
		if off < tableEnd || off+n > size {
			return fmt.Errorf("%w: ICC tag %d at %d+%d is outside the profile", ErrMalformedMetadata, i, off, n)
		}
	}
	// `curv` はタグの中 (mAB / mBA / mft の中に埋め込まれた曲線を含む) で要素数の
	// ぶんを先に確保してから読むので、出現位置ごとに残りバイト数と比べる。
	// 埋め込み位置を構造から辿る代わりに、4 バイト境界の "curv" を全て見る
	// (偶然の一致で実在のプロファイルを落とすことは実用上無い)。
	for i := 132; i+12 <= len(p); i += 4 {
		if string(p[i:i+4]) != "curv" {
			continue
		}
		n := uint64(binary.BigEndian.Uint32(p[i+8 : i+12]))
		if 12+n*2 > uint64(len(p)-i) {
			return fmt.Errorf("%w: ICC curve at %d declares %d points", ErrMalformedMetadata, i, n)
		}
	}
	return nil
}
