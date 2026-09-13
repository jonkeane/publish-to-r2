// Package photometadata reads allowlisted public metadata from a finished JPEG.
package photometadata

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

const analogNamespace = "http://analogexif.sourceforge.net/ns"
const rdfNamespace = "http://www.w3.org/1999/02/22-rdf-syntax-ns#"
const xmpHeader = "http://ns.adobe.com/xap/1.0/\x00"

// Emulsion reads AnalogExif:Film from standard XMP APP1 packets before the
// image scan. Namespace URIs, not arbitrary XML prefixes, identify the field.
// Missing metadata is empty; malformed or conflicting metadata is an error.
// Extended XMP is not supported. The writer must keep this small scalar in
// standard XMP. Header reads are bounded independently of the JPEG pixel size.
func Emulsion(input io.Reader) (string, error) {
	film := ""
	err := walkAPP1(input, func(payload []byte) error {
		if !bytes.HasPrefix(payload, []byte(xmpHeader)) {
			return nil
		}
		value, err := readFilmXMP(payload[len(xmpHeader):])
		if err != nil {
			return err
		}
		return mergeFilm(&film, value)
	})
	if err != nil {
		return "", err
	}
	return film, nil
}

func walkAPP1(input io.Reader, visit func([]byte) error) error {
	r := bufio.NewReader(io.LimitReader(input, 8<<20))
	var signature [2]byte
	if _, err := io.ReadFull(r, signature[:]); err != nil || signature != [2]byte{0xff, 0xd8} {
		return errors.New("invalid JPEG header")
	}
	for {
		b, err := r.ReadByte()
		if err != nil || b != 0xff {
			return errors.New("invalid or oversized JPEG metadata header")
		}
		for b == 0xff {
			b, err = r.ReadByte()
			if err != nil {
				return errors.New("truncated JPEG marker")
			}
		}
		if b == 0xda || b == 0xd9 { // Start of scan or end of image.
			return nil
		}
		if b == 0x00 || b == 0xd8 {
			return errors.New("invalid JPEG marker")
		}
		if b == 0x01 || (b >= 0xd0 && b <= 0xd7) { // Standalone markers.
			continue
		}
		var size [2]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return errors.New("truncated JPEG segment")
		}
		n := int(binary.BigEndian.Uint16(size[:])) - 2
		if n < 0 {
			return errors.New("invalid JPEG segment length")
		}
		if b != 0xe1 {
			if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
				return errors.New("truncated JPEG segment")
			}
			continue
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return errors.New("truncated JPEG metadata")
		}
		if err := visit(payload); err != nil {
			return err
		}
	}
}

func mergeFilm(film *string, value string) error {
	value = strings.TrimSpace(value)
	if len(value) > 4096 {
		return errors.New("XMP film name exceeds 4096 bytes")
	}
	if value == "" {
		return nil
	}
	if *film != "" && *film != value {
		return errors.New("conflicting XMP film names")
	}
	*film = value
	return nil
}

func readFilmXMP(packet []byte) (string, error) {
	d := xml.NewDecoder(bytes.NewReader(packet))
	var stack []xml.Name
	descriptionDepth := 0
	film := ""
	for {
		token, err := d.Token()
		if err == io.EOF {
			return film, nil
		}
		if err != nil {
			return "", errors.New("invalid film XMP packet")
		}
		switch t := token.(type) {
		case xml.StartElement:
			if descriptionDepth > 0 && len(stack) == descriptionDepth && t.Name == (xml.Name{Space: analogNamespace, Local: "Film"}) {
				var value strings.Builder
				for {
					part, err := d.Token()
					if err != nil {
						return "", errors.New("invalid film XMP value")
					}
					if _, end := part.(xml.EndElement); end {
						break
					}
					switch p := part.(type) {
					case xml.CharData:
						value.Write(p)
					case xml.StartElement:
						return "", errors.New("XMP film name must be a scalar")
					}
				}
				if err := mergeFilm(&film, value.String()); err != nil {
					return "", err
				}
				continue
			}
			if len(stack) > 0 && stack[len(stack)-1] == (xml.Name{Space: rdfNamespace, Local: "RDF"}) && t.Name == (xml.Name{Space: rdfNamespace, Local: "Description"}) {
				// Named RDF resources describe something other than this image.
				isImage := true
				for _, a := range t.Attr {
					if a.Name == (xml.Name{Space: rdfNamespace, Local: "about"}) && a.Value != "" {
						isImage = false
					}
				}
				if isImage {
					descriptionDepth = len(stack) + 1
					for _, a := range t.Attr {
						if a.Name == (xml.Name{Space: analogNamespace, Local: "Film"}) {
							if err := mergeFilm(&film, a.Value); err != nil {
								return "", err
							}
						}
					}
				}
			}
			stack = append(stack, t.Name)
		case xml.EndElement:
			if len(stack) == descriptionDepth {
				descriptionDepth = 0
			}
			stack = stack[:len(stack)-1]
		}
	}
}
