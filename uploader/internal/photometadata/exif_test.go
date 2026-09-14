package photometadata

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
)

func TestReadExifToolTechnicalFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/technical.jpg")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(bytes.NewReader(b))
	want := manifest.EXIF{Emulsion: "Kodak Portra 400", Make: "Not film stock", Model: "Film Camera", Lens: "50mm f/1.4", FocalLength: "50 mm", FStop: "f/5.6", Exposure: "1/125", ISO: "800", Time: "2025-07-06T14:03:02.125-05:00", Flash: "Fired"}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	encoded, _ := json.Marshal(got)
	for _, private := range []string{"41.88", "87.63", "GPS"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("unselected metadata leaked: %s", encoded)
		}
	}
}

type testTag struct {
	id, kind uint16
	count    uint32
	value    []byte
}

func textTag(id uint16, s string) testTag {
	return testTag{id, 2, uint32(len(s) + 1), append([]byte(s), 0)}
}
func intTag(order binary.ByteOrder, id uint16, value uint32) testTag {
	data := make([]byte, 4)
	order.PutUint32(data, value)
	return testTag{id, 4, 1, data}
}
func ratTag(order binary.ByteOrder, id uint16, a, b uint32) testTag {
	data := make([]byte, 8)
	order.PutUint32(data, a)
	order.PutUint32(data[4:], b)
	return testTag{id, 5, 1, data}
}
func testTIFF(order binary.ByteOrder, primary, technical []testTag) []byte {
	offset := uint32(8 + 2 + (len(primary)+1)*12 + 4)
	primary = append(primary, intTag(order, 0x8769, offset))
	data := make([]byte, int(offset)+2+len(technical)*12+4)
	if order == binary.LittleEndian {
		copy(data, "II")
	} else {
		copy(data, "MM")
	}
	order.PutUint16(data[2:], 42)
	order.PutUint32(data[4:], 8)
	write := func(at int, tags []testTag) {
		order.PutUint16(data[at:], uint16(len(tags)))
		for i, tag := range tags {
			e := data[at+2+i*12 : at+2+(i+1)*12]
			order.PutUint16(e, tag.id)
			order.PutUint16(e[2:], tag.kind)
			order.PutUint32(e[4:], tag.count)
			if len(tag.value) <= 4 {
				copy(e[8:], tag.value)
			} else {
				order.PutUint32(e[8:], uint32(len(data)))
				data = append(data, tag.value...)
			}
		}
	}
	write(8, primary)
	write(int(offset), technical)
	return data
}
func exifJPEG(packets ...[]byte) []byte {
	b := []byte{0xff, 0xd8}
	for _, p := range packets {
		b = append(b, 0xff, 0xe1)
		b = binary.BigEndian.AppendUint16(b, uint16(len(exifHeader)+len(p)+2))
		b = append(b, exifHeader...)
		b = append(b, p...)
	}
	return append(b, 0xff, 0xda)
}
func TestEXIFByteOrdersAndOptionalFields(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		t.Run(order.String(), func(t *testing.T) {
			data := testTIFF(order, []testTag{textTag(0x010f, "Digital maker"), textTag(0x0110, "Digital camera")}, []testTag{
				ratTag(order, 0x829a, 5, 2), ratTag(order, 0x829d, 28, 10), ratTag(order, 0x920a, 515, 10),
				intTag(order, 0x8827, 65535), intTag(order, 0x8830, 3), intTag(order, 0x8833, 102400),
				intTag(order, 0x9209, 0), textTag(0x9003, "2026:09:11 01:02:03"),
				// GPS/thumbnail/MakerNote pointers are deliberately invalid and must not be followed.
				intTag(order, 0x927c, 0xffffffff),
			})
			got, err := Read(bytes.NewReader(exifJPEG(data)))
			want := manifest.EXIF{Make: "Digital maker", Model: "Digital camera", Exposure: "2.5", FStop: "f/2.8", FocalLength: "51.5 mm", ISO: "102400", Flash: "Did not fire", Time: "2026-09-11T01:02:03"}
			if err != nil || got != want {
				t.Fatalf("got %+v, %v; want %+v", got, err, want)
			}
		})
	}
	got, err := Read(bytes.NewReader(jpegWith()))
	if err != nil || got != (manifest.EXIF{}) {
		t.Fatalf("missing: %+v, %v", got, err)
	}
	film, err := Read(bytes.NewReader(jpegWith(packet(`f:Film="Stock"/>`))))
	if err != nil || film != (manifest.EXIF{Emulsion: "Stock"}) {
		t.Fatalf("XMP only: %+v, %v", film, err)
	}
}
func TestOriginalTime(t *testing.T) {
	for _, tc := range []struct {
		date, subsec, offset, want string
		bad                        bool
	}{
		{"2025:01:02 03:04:05", "", "", "2025-01-02T03:04:05", false},
		{"2025:01:02 03:04:05", "001", "+05:30", "2025-01-02T03:04:05.001+05:30", false},
		{"2025:01:02 03:04:05", "", "+00:00", "2025-01-02T03:04:05+00:00", false},
		{"", "", "", "", false}, {"0000:00:00 00:00:00", "", "", "", false}, {"    :  :     :  :  ", "", "", "", false},
		{"2025:02:30 03:04:05", "", "", "", true}, {"2025:01:02 03:04:05", "x", "", "", true},
		{"2025:01:02 03:04:05", "", "+24:00", "", true}, {"2025:01:02 03:04:05", "", "+05:99", "", true},
	} {
		got, err := originalTime(tc.date, tc.subsec, tc.offset)
		if got != tc.want || (err != nil) != tc.bad {
			t.Fatalf("%+v: got %q, %v", tc, got, err)
		}
	}
}
func TestMalformedEXIF(t *testing.T) {
	order := binary.LittleEndian
	good := testTIFF(order, []testTag{textTag(0x0110, "Camera")}, nil)
	offset := append([]byte(nil), good...)
	order.PutUint32(offset[4:], 0xffffffff)
	valueOffset := append([]byte(nil), good...)
	order.PutUint32(valueOffset[18:], 0xffffffff)
	badOrder := append([]byte(nil), good...)
	copy(badOrder, "ZZ")
	hugeCount := append([]byte(nil), good...)
	order.PutUint32(hugeCount[14:], 0xffffffff)
	cases := [][]byte{
		{}, good[:7], good[:10], offset, valueOffset, badOrder, hugeCount,
		testTIFF(order, []testTag{textTag(0x0110, "One"), textTag(0x0110, "Two")}, nil),
		testTIFF(order, []testTag{textTag(0x0110, strings.Repeat("x", 4097))}, nil),
		testTIFF(order, []testTag{intTag(order, 0x0110, 42)}, nil),
		testTIFF(order, nil, []testTag{ratTag(order, 0x829a, 1, 0)}),
		testTIFF(order, nil, []testTag{textTag(0x9003, "not a date")}),
	}
	for i, data := range cases {
		if got, err := Read(bytes.NewReader(exifJPEG(data))); err == nil || got != (manifest.EXIF{}) {
			t.Fatalf("malformed %d: %+v, %v", i, got, err)
		}
	}
	if _, err := Read(bytes.NewReader(exifJPEG(good, good))); err != nil {
		t.Fatal(err)
	}
	different := testTIFF(order, []testTag{textTag(0x0110, "Other")}, nil)
	if _, err := Read(bytes.NewReader(exifJPEG(good, different))); err == nil {
		t.Fatal("conflicting EXIF packets accepted")
	}
	// A different original time tag or thumbnail date is not DateTimeOriginal.
	absent := testTIFF(order, []testTag{textTag(0x0132, "2025:01:02 03:04:05")}, []testTag{textTag(0x9004, "2025:01:02 03:04:05")})
	if got, err := Read(bytes.NewReader(exifJPEG(absent))); err != nil || got.Time != "" {
		t.Fatalf("substituted non-capture date: %+v, %v", got, err)
	}
}
func FuzzRead(f *testing.F) {
	b, err := os.ReadFile("testdata/technical.jpg")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
	f.Add(jpegWith())
	f.Add(exifJPEG(testTIFF(binary.LittleEndian, nil, nil)))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = Read(bytes.NewReader(b)) })
}
