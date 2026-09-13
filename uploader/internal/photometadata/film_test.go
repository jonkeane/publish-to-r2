package photometadata

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

func packet(body string) string {
	return `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="` + rdfNamespace + `"><rdf:Description rdf:about="" xmlns:f="` + analogNamespace + `" ` + body + `</rdf:RDF></x:xmpmeta>`
}

func jpegWith(packets ...string) []byte {
	b := []byte{0xff, 0xd8}
	for _, p := range packets {
		payload := []byte(xmpHeader + p)
		b = append(b, 0xff, 0xe1)
		b = binary.BigEndian.AppendUint16(b, uint16(len(payload)+2))
		b = append(b, payload...)
	}
	return append(b, 0xff, 0xda)
}

func TestEmulsion(t *testing.T) {
	cases := []struct {
		name string
		jpeg []byte
		want string
		bad  bool
	}{
		{"missing", jpegWith(), "", false},
		{"element", jpegWith(packet(`><f:Film> Kodak Portra 400 </f:Film></rdf:Description>`)), "Kodak Portra 400", false},
		{"attribute", jpegWith(packet(`f:Film="日本 &amp; Film"/>`)), "日本 & Film", false},
		{"wrong namespace", jpegWith(strings.ReplaceAll(packet(`f:Film="Wrong"/>`), analogNamespace, "https://example.com/other")), "", false},
		{"camera make", jpegWith(packet(`xmlns:tiff="http://ns.adobe.com/tiff/1.0/" tiff:Make="Kodak"/>`)), "", false},
		{"other resource", jpegWith(strings.Replace(packet(`f:Film="Wrong"/>`), `rdf:about=""`, `rdf:about="other"`, 1)), "", false},
		{"nested object", jpegWith(packet(`><f:Other f:Film="Wrong"/></rdf:Description>`)), "", false},
		{"duplicate same", jpegWith(packet(`f:Film="Stock"/>`), packet(`><f:Film>Stock</f:Film></rdf:Description>`)), "Stock", false},
		{"conflict", jpegWith(packet(`f:Film="Stock"/>`), packet(`f:Film="Other"/>`)), "", true},
		{"malformed XML", jpegWith(packet(`><f:Film>Stock</rdf:Description>`)), "", true},
		{"not scalar", jpegWith(packet(`><f:Film><f:Other>Stock</f:Other></f:Film></rdf:Description>`)), "", true},
		{"oversized field", jpegWith(packet(`f:Film="` + strings.Repeat("a", 4097) + `"/>`)), "", true},
		{"truncated segment", []byte{0xff, 0xd8, 0xff, 0xe1, 0x00, 0xff}, "", true},
		{"invalid length", []byte{0xff, 0xd8, 0xff, 0xe1, 0x00, 0x01}, "", true},
		{"non JPEG", []byte("not a jpeg"), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Emulsion(bytes.NewReader(tc.jpeg))
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("got %q, %v; want %q, error=%v", got, err, tc.want, tc.bad)
			}
		})
	}
}

func TestExifToolFixture(t *testing.T) {
	f, err := os.Open("testdata/film.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	film, err := Emulsion(f)
	if err != nil || film != "Kodak Portra 400" {
		t.Fatalf("ExifTool fixture: %q, %v", film, err)
	}
}

func FuzzEmulsion(f *testing.F) {
	f.Add(jpegWith(packet(`f:Film="Stock"/>`)))
	f.Add([]byte{0xff, 0xd8, 0xff, 0xe1, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = Emulsion(bytes.NewReader(b)) })
}
