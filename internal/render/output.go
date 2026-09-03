package render

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"image/png"
	"io"
	"math"
	"strconv"
	"strings"
)

var pngIEND = []byte("\x00\x00\x00\x00IEND\xaeB`\x82")

func sanitizeOutput(format Format, data []byte) ([]byte, error) {
	if format != FormatSVG {
		return data, nil
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var sanitized bytes.Buffer
	copyStart := 0
	skipDepth := 0
	changed := false
	for {
		start := int(decoder.InputOffset())
		token, err := decoder.Token()
		end := int(decoder.InputOffset())
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, &Error{Code: CodeInternal, Message: "renderer produced an invalid SVG", Cause: err}
		}
		switch value := token.(type) {
		case xml.StartElement:
			if skipDepth > 0 {
				skipDepth++
			} else if strings.EqualFold(value.Name.Local, "foreignObject") {
				sanitized.Write(data[copyStart:start])
				skipDepth = 1
				changed = true
			}
		case xml.EndElement:
			if skipDepth > 0 {
				skipDepth--
				if skipDepth == 0 {
					copyStart = end
				}
			}
		}
	}
	if skipDepth != 0 {
		return nil, &Error{Code: CodeInternal, Message: "renderer produced an invalid SVG"}
	}
	if !changed {
		return data, nil
	}
	sanitized.Write(data[copyStart:])
	return sanitized.Bytes(), nil
}

func validateOutput(format Format, data []byte, maxBytes int64) error {
	if len(data) == 0 {
		return &Error{Code: CodeInternal, Message: "renderer produced an invalid image"}
	}
	if int64(len(data)) > maxBytes {
		return &Error{
			Code:    CodeTooLarge,
			Message: fmt.Sprintf("rendered image exceeds the %d-byte limit", maxBytes),
		}
	}
	switch format {
	case FormatPNG:
		return validatePNG(data)
	case FormatSVG:
		return validateSVG(data)
	default:
		return &Error{Code: CodeInternal, Message: "renderer produced an invalid image"}
	}
}

func validatePNG(data []byte) error {
	const signatureSize = 8
	if len(data) < signatureSize || !bytes.Equal(data[:signatureSize], []byte("\x89PNG\r\n\x1a\n")) {
		return invalidImageError(nil)
	}
	offset := signatureSize
	seenIHDR := false
	for offset < len(data) {
		if len(data)-offset < 12 {
			return invalidImageError(nil)
		}
		length := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
		if length > uint64(len(data)-offset-12) {
			return invalidImageError(nil)
		}
		chunkEnd := offset + 12 + int(length)
		chunkType := data[offset+4 : offset+8]
		chunkData := data[offset+8 : offset+8+int(length)]
		wantCRC := binary.BigEndian.Uint32(data[offset+8+int(length) : chunkEnd])
		crc := crc32.NewIEEE()
		_, _ = crc.Write(chunkType)
		_, _ = crc.Write(chunkData)
		if crc.Sum32() != wantCRC {
			return invalidImageError(nil)
		}
		if !seenIHDR {
			if !bytes.Equal(chunkType, []byte("IHDR")) || length != 13 {
				return invalidImageError(nil)
			}
			width := binary.BigEndian.Uint32(chunkData[:4])
			height := binary.BigEndian.Uint32(chunkData[4:8])
			if err := validateDimensions(float64(width), float64(height)); err != nil {
				return err
			}
			seenIHDR = true
		}
		if bytes.Equal(chunkType, []byte("IEND")) {
			if length != 0 || chunkEnd != len(data) {
				return invalidImageError(nil)
			}
			if _, err := png.DecodeConfig(bytes.NewReader(data)); err != nil {
				return invalidImageError(err)
			}
			return nil
		}
		offset = chunkEnd
	}
	return invalidImageError(nil)
}

func validateSVG(data []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	seenRoot := false
	styleDepth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &Error{Code: CodeInternal, Message: "renderer produced an invalid SVG", Cause: err}
		}
		switch value := token.(type) {
		case xml.StartElement:
			name := strings.ToLower(value.Name.Local)
			if !seenRoot {
				if name != "svg" || (value.Name.Space != "" && value.Name.Space != "http://www.w3.org/2000/svg") {
					return unsafeSVGError(nil)
				}
				if err := validateSVGDimensions(value.Attr); err != nil {
					return err
				}
				seenRoot = true
			}
			switch name {
			case "script", "foreignobject", "iframe", "object", "embed", "audio", "video",
				"animate", "animatemotion", "animatetransform", "set", "discard":
				return unsafeSVGError(nil)
			case "style":
				styleDepth++
			}
			for _, attr := range value.Attr {
				attrName := strings.ToLower(attr.Name.Local)
				attrValue := strings.TrimSpace(attr.Value)
				if strings.HasPrefix(attrName, "on") || attrName == "base" {
					return unsafeSVGError(nil)
				}
				if (attrName == "href" || attrName == "src") && attrValue != "" && !strings.HasPrefix(attrValue, "#") {
					return unsafeSVGError(nil)
				}
				if unsafeURLReference(attrValue) || (attrName == "style" && unsafeCSS(attrValue)) {
					return unsafeSVGError(nil)
				}
			}
		case xml.EndElement:
			if strings.EqualFold(value.Name.Local, "style") && styleDepth > 0 {
				styleDepth--
			}
		case xml.CharData:
			if styleDepth > 0 && unsafeCSS(string(value)) {
				return unsafeSVGError(nil)
			}
		case xml.Directive:
			return unsafeSVGError(nil)
		case xml.ProcInst:
			if !strings.EqualFold(value.Target, "xml") {
				return unsafeSVGError(nil)
			}
		}
	}
	if !seenRoot {
		return unsafeSVGError(nil)
	}
	return nil
}

func validateSVGDimensions(attributes []xml.Attr) error {
	var width, height float64
	for _, attribute := range attributes {
		switch strings.ToLower(attribute.Name.Local) {
		case "width":
			width = parseSVGDimension(attribute.Value)
		case "height":
			height = parseSVGDimension(attribute.Value)
		case "viewbox":
			fields := strings.Fields(strings.ReplaceAll(attribute.Value, ",", " "))
			if len(fields) != 4 {
				return invalidImageError(nil)
			}
			parsedWidth, widthErr := strconv.ParseFloat(fields[2], 64)
			parsedHeight, heightErr := strconv.ParseFloat(fields[3], 64)
			if widthErr != nil || heightErr != nil {
				return invalidImageError(nil)
			}
			if err := validateDimensions(parsedWidth, parsedHeight); err != nil {
				return err
			}
		}
	}
	if width > 0 && height > 0 {
		return validateDimensions(width, height)
	}
	if width > maxOutputDimension || height > maxOutputDimension {
		return outputDimensionsError()
	}
	return nil
}

func parseSVGDimension(value string) float64 {
	value = strings.TrimSpace(value)
	if strings.HasSuffix(value, "%") {
		return 0
	}
	value = strings.TrimSuffix(value, "px")
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return parsed
}

const (
	maxOutputDimension = 8192
	maxOutputPixels    = 16_777_216
)

func validateDimensions(width, height float64) error {
	if width <= 0 || height <= 0 || math.IsNaN(width) || math.IsNaN(height) || math.IsInf(width, 0) || math.IsInf(height, 0) {
		return invalidImageError(nil)
	}
	if width > maxOutputDimension || height > maxOutputDimension || width*height > maxOutputPixels {
		return outputDimensionsError()
	}
	return nil
}

func outputDimensionsError() error {
	return &Error{Code: CodeTooLarge, Message: "rendered image dimensions exceed the safety limit"}
}

func invalidImageError(cause error) error {
	return &Error{Code: CodeInternal, Message: "renderer produced an invalid image", Cause: cause}
}

func unsafeCSS(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "\\") || strings.Contains(lower, "expression(") ||
		strings.Contains(lower, "@import") || strings.Contains(lower, "image-set(") ||
		strings.Contains(lower, "-moz-binding") || unsafeURLReference(value)
}

func unsafeURLReference(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "javascript:") {
		return true
	}
	for offset := 0; ; {
		index := strings.Index(lower[offset:], "url(")
		if index < 0 {
			return false
		}
		start := offset + index + len("url(")
		endOffset := strings.IndexByte(lower[start:], ')')
		if endOffset < 0 {
			return true
		}
		reference := strings.Trim(strings.TrimSpace(lower[start:start+endOffset]), "'\"")
		if !strings.HasPrefix(reference, "#") {
			return true
		}
		offset = start + endOffset + 1
	}
}

func unsafeSVGError(cause error) error {
	return &Error{Code: CodeRenderRejected, Message: "rendered SVG violates the safety policy", Cause: cause}
}
