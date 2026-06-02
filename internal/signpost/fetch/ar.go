package fetch

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	arGlobalMagic = "!<arch>\n"
	arHeaderSize  = 60
)

// ArMember describes a single member's position within the parent ar file.
// Offset is the absolute offset of the member's data (i.e. start of body,
// not header); Size is the declared body length.
type ArMember struct {
	Name   string // null-padded name with trailing slash stripped
	Offset int64
	Size   int64
}

// ParseARHeaders walks the ar archive in buf, returning every member it can
// see. Members whose body extends past len(buf) are still returned — Offset
// and Size are set, but the caller is expected to range-fetch the missing
// bytes. Walking stops at the first malformed header.
func ParseARHeaders(buf []byte) ([]ArMember, error) {
	if len(buf) < len(arGlobalMagic) {
		return nil, errors.New("ar: truncated, missing global magic")
	}
	if string(buf[:len(arGlobalMagic)]) != arGlobalMagic {
		return nil, fmt.Errorf("ar: bad global magic %q", buf[:len(arGlobalMagic)])
	}

	pos := int64(len(arGlobalMagic))
	var members []ArMember
	for {
		// We must have a full 60-byte header in the buffer.
		if pos+arHeaderSize > int64(len(buf)) {
			break
		}
		hdr := buf[pos : pos+arHeaderSize]
		// Trailer bytes 58-59 must be 0x60 0x0a.
		if hdr[58] != 0x60 || hdr[59] != 0x0a {
			return nil, fmt.Errorf("ar: bad header trailer at offset %d", pos)
		}
		name := strings.TrimRight(string(hdr[0:16]), " ")
		name = strings.TrimSuffix(name, "/")

		sizeStr := strings.TrimRight(string(hdr[48:58]), " ")
		size, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ar: bad size %q at offset %d: %w", sizeStr, pos, err)
		}

		bodyOffset := pos + arHeaderSize
		members = append(members, ArMember{
			Name:   name,
			Offset: bodyOffset,
			Size:   size,
		})

		// Advance past body, with even-length padding.
		pos = bodyOffset + size
		if size%2 != 0 {
			pos++
		}
	}
	return members, nil
}

// FindControlMember returns the ar member representing control.tar.{gz,xz,zst}.
// Per Debian policy this is always the second member, after debian-binary.
func FindControlMember(members []ArMember) (ArMember, error) {
	for _, m := range members {
		if strings.HasPrefix(m.Name, "control.tar") {
			return m, nil
		}
	}
	return ArMember{}, errors.New("ar: no control.tar.* member found")
}

// SliceMember returns the body bytes for m from buf, or false if buf does
// not fully cover the member.
func SliceMember(buf []byte, m ArMember) ([]byte, bool) {
	end := m.Offset + m.Size
	if end > int64(len(buf)) {
		return nil, false
	}
	return buf[m.Offset:end], true
}

// MustHaveARMagic returns an error unless buf starts with the ar global magic.
// Useful for early-failing on responses that aren't .deb files.
func MustHaveARMagic(buf []byte) error {
	if !bytes.HasPrefix(buf, []byte(arGlobalMagic)) {
		head := buf
		if len(head) > 16 {
			head = head[:16]
		}
		return fmt.Errorf("ar: response does not start with ar magic (got %q)", head)
	}
	return nil
}
