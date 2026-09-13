package photometadata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jkeane/publish-to-r2/uploader/internal/manifest"
)

const exifHeader = "Exif\x00\x00"

// Read reads standard EXIF from the primary image's IFD0/ExifIFD and film
// stock from standard XMP. It never follows GPS, MakerNote, or thumbnail IFDs.
// Missing tags stay empty. All reads are bounded by the JPEG APP1 segment and
// the metadata header limit in walkAPP1; malformed selected tags fail closed.
func Read(input io.Reader) (manifest.EXIF, error) {
	var result manifest.EXIF
	var seenEXIF []byte
	err := walkAPP1(input, func(payload []byte) error {
		switch {
		case bytes.HasPrefix(payload, []byte(exifHeader)):
			if seenEXIF != nil {
				if !bytes.Equal(seenEXIF, payload) {
					return errors.New("conflicting EXIF packets")
				}
				return nil
			}
			exif, err := readEXIF(payload[len(exifHeader):])
			if err != nil {
				return err
			}
			exif.Emulsion = result.Emulsion
			result = exif
			seenEXIF = payload
		case bytes.HasPrefix(payload, []byte(xmpHeader)):
			film, err := readFilmXMP(payload[len(xmpHeader):])
			if err != nil {
				return err
			}
			return mergeFilm(&result.Emulsion, film)
		}
		return nil
	})
	if err != nil {
		return manifest.EXIF{}, err
	}
	return result, nil
}

type tiffReader struct {
	data  []byte
	order binary.ByteOrder
}
type exifValue struct {
	kind  uint16
	count uint32
	data  []byte
	order binary.ByteOrder
}

// Only these tags can contribute to public technical metadata. Tag types follow
// CIPA DC-008 (Exif); type 129 is the UTF-8 string type added in Exif 3.0.
var primaryTags = map[uint16]bool{0x010f: true, 0x0110: true, 0x8769: true}
var technicalTags = map[uint16]bool{
	0x829a: true, 0x829d: true, 0x8827: true, 0x8830: true, 0x8831: true, 0x8832: true, 0x8833: true,
	0x9003: true, 0x9011: true, 0x9209: true, 0x920a: true, 0x9291: true, 0xa434: true,
}

func (r tiffReader) ifd(offset uint32, allowed map[uint16]bool) (map[uint16]exifValue, error) {
	pos := uint64(offset)
	if pos < 8 || pos+2 > uint64(len(r.data)) {
		return nil, errors.New("invalid EXIF IFD offset")
	}
	count := uint64(r.order.Uint16(r.data[pos : pos+2]))
	if pos+2+count*12+4 > uint64(len(r.data)) {
		return nil, errors.New("truncated EXIF IFD")
	}
	values := map[uint16]exifValue{}
	for i := uint64(0); i < count; i++ {
		entry := r.data[pos+2+i*12 : pos+2+(i+1)*12]
		tag := r.order.Uint16(entry)
		if !allowed[tag] {
			continue
		}
		kind, n := r.order.Uint16(entry[2:]), r.order.Uint32(entry[4:])
		var width uint64
		switch kind {
		case 2, 129:
			width = 1
		case 3:
			width = 2
		case 4:
			width = 4
		case 5:
			width = 8
		default:
			return nil, fmt.Errorf("unsupported EXIF type for tag %04x", tag)
		}
		size := uint64(n) * width
		if size == 0 {
			continue
		}
		data := entry[8:12]
		if size > 4 {
			start := uint64(r.order.Uint32(data))
			if start < 8 || start+size > uint64(len(r.data)) {
				return nil, errors.New("invalid EXIF value offset or size")
			}
			data = r.data[start : start+size]
		} else {
			data = data[:size]
		}
		if _, exists := values[tag]; exists {
			return nil, fmt.Errorf("duplicate EXIF tag %04x", tag)
		}
		values[tag] = exifValue{kind, n, data, r.order}
	}
	return values, nil
}

func (v exifValue) text() (string, error) {
	if v.count == 0 {
		return "", nil
	}
	if v.kind != 2 && v.kind != 129 {
		return "", errors.New("invalid EXIF text type")
	}
	s := strings.TrimSpace(strings.TrimRight(string(v.data), "\x00"))
	if len(s) > 4096 || !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return "", errors.New("invalid or oversized EXIF text")
	}
	return s, nil
}
func (v exifValue) integer() (uint32, error) {
	if v.count == 0 {
		return 0, nil
	}
	if v.count != 1 {
		return 0, errors.New("invalid EXIF integer count")
	}
	switch v.kind {
	case 3:
		return uint32(v.order.Uint16(v.data)), nil
	case 4:
		return v.order.Uint32(v.data), nil
	}
	return 0, errors.New("invalid EXIF integer type")
}
func (v exifValue) rational() (uint32, uint32, error) {
	if v.count == 0 {
		return 0, 1, nil
	}
	if v.kind != 5 || v.count != 1 {
		return 0, 0, errors.New("invalid EXIF rational type or count")
	}
	a, b := v.order.Uint32(v.data), v.order.Uint32(v.data[4:])
	if b == 0 {
		return 0, 0, errors.New("invalid EXIF rational denominator")
	}
	return a, b, nil
}
func decimal(a, b uint32) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(float64(a)/float64(b), 'f', 4, 64), "0"), ".")
}

func readEXIF(data []byte) (manifest.EXIF, error) {
	var result manifest.EXIF
	if len(data) < 8 {
		return result, errors.New("truncated EXIF header")
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return result, errors.New("invalid EXIF byte order")
	}
	if order.Uint16(data[2:]) != 42 {
		return result, errors.New("invalid EXIF TIFF header")
	}
	reader := tiffReader{data, order}
	primary, err := reader.ifd(order.Uint32(data[4:]), primaryTags)
	if err != nil {
		return result, err
	}
	if result.Make, err = primary[0x010f].text(); err != nil {
		return result, err
	}
	if result.Model, err = primary[0x0110].text(); err != nil {
		return result, err
	}
	offset, err := primary[0x8769].integer()
	if err != nil {
		return result, err
	}
	if offset == 0 {
		return result, nil
	}
	if offset == order.Uint32(data[4:]) {
		return result, errors.New("cyclic EXIF IFD pointer")
	}
	tags, err := reader.ifd(offset, technicalTags)
	if err != nil {
		return result, err
	}
	if result.Lens, err = tags[0xa434].text(); err != nil {
		return result, err
	}
	for _, field := range []struct {
		tag            uint16
		dest           *string
		prefix, suffix string
	}{
		{0x920a, &result.FocalLength, "", " mm"}, {0x829d, &result.FStop, "f/", ""}, {0x829a, &result.Exposure, "", ""},
	} {
		v := tags[field.tag]
		a, b, err := v.rational()
		if err != nil {
			return result, err
		}
		if v.count == 0 || a == 0 {
			continue
		}
		value := decimal(a, b)
		if field.tag == 0x829a && a < b {
			// Preserve the actual rational exposure; don't round 1/125 to a decimal.
			x, y := a, b
			for y != 0 {
				x, y = y, x%y
			}
			value = fmt.Sprintf("%d/%d", a/x, b/x)
		}
		*field.dest = field.prefix + value + field.suffix
	}
	iso, err := tags[0x8827].integer()
	if err != nil {
		return result, err
	}
	if iso == 0 || iso == 65535 {
		// EXIF 2.3 stores higher sensitivities in LONG tags. SensitivityType tells
		// which of SOS, REI, and ISO speed the short value represents.
		kind, err := tags[0x8830].integer()
		if err != nil {
			return result, err
		}
		var tag uint16
		switch kind {
		case 1:
			tag = 0x8831
		case 2, 4:
			tag = 0x8832
		case 3, 5, 6, 7:
			tag = 0x8833
		}
		iso = 0
		if tag != 0 {
			iso, err = tags[tag].integer()
			if err != nil {
				return result, err
			}
		}
	}
	if iso != 0 {
		result.ISO = strconv.FormatUint(uint64(iso), 10)
	}
	if v := tags[0x9209]; v.count != 0 {
		flash, err := v.integer()
		if err != nil {
			return result, err
		}
		result.Flash = flashDescription(flash)
	}
	date, err := tags[0x9003].text()
	if err != nil {
		return result, err
	}
	subsec, err := tags[0x9291].text()
	if err != nil {
		return result, err
	}
	offsetText, err := tags[0x9011].text()
	if err != nil {
		return result, err
	}
	result.Time, err = originalTime(date, subsec, offsetText)
	return result, err
}

var digits = regexp.MustCompile(`^[0-9]+$`)
var zoneOffset = regexp.MustCompile(`^[+-](?:0[0-9]|1[0-9]|2[0-3]):[0-5][0-9]$`)

func originalTime(date, subsec, offset string) (string, error) {
	// EXIF explicitly permits blank/unknown dates. Never substitute digitization
	// time, file mtime, the computer's timezone, or a catalog timestamp.
	if strings.Trim(date, " :0") == "" {
		return "", nil
	}
	parsed, err := time.Parse("2006:01:02 15:04:05", date)
	if err != nil {
		return "", errors.New("invalid EXIF original capture time")
	}
	value := parsed.Format("2006-01-02T15:04:05")
	if subsec != "" {
		if !digits.MatchString(subsec) {
			return "", errors.New("invalid EXIF original subsecond time")
		}
		value += "." + subsec
	}
	if offset != "" {
		if !zoneOffset.MatchString(offset) {
			return "", errors.New("invalid EXIF original time offset")
		}
		value += offset
	}
	return value, nil
}
func flashDescription(value uint32) string {
	if value&32 != 0 {
		return "No flash function"
	}
	text := "Did not fire"
	if value&1 != 0 {
		text = "Fired"
	}
	switch (value >> 3) & 3 {
	case 1:
		text += ", compulsory mode"
	case 2:
		text += ", suppressed"
	case 3:
		text += ", auto mode"
	}
	switch (value >> 1) & 3 {
	case 2:
		text += ", return not detected"
	case 3:
		text += ", return detected"
	}
	if value&64 != 0 {
		text += ", red-eye reduction"
	}
	return text
}
