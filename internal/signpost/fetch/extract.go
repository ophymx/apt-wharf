package fetch

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"pault.ag/go/debian/deb"
)

// DecompressControlTar opens the right decompressor for the named member
// (control.tar.gz, control.tar.xz, control.tar.zst, etc.) and returns a
// reader of the inner tar bytes.
//
// Delegates compression-format selection to pault.ag/go/debian/deb so we
// inherit support for every algorithm Debian/Ubuntu expects (gz, xz, zst,
// bz2, lzma) without maintaining our own switch.
func DecompressControlTar(memberName string, body []byte) (io.Reader, error) {
	if memberName == "control.tar" {
		return bytes.NewReader(body), nil
	}
	if !strings.HasPrefix(memberName, "control.tar.") {
		return nil, fmt.Errorf("unsupported control member %q", memberName)
	}
	ext := filepath.Ext(memberName) // ".gz" / ".xz" / ".zst" / ...
	rc, err := deb.DecompressorFor(ext)(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s decompress: %w", ext, err)
	}
	return rc, nil
}

// ReadControlFromTar walks tar entries looking for ./control or control,
// returning its raw bytes. Other members are skipped.
func ReadControlFromTar(r io.Reader) ([]byte, error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("control file not found in control.tar")
		}
		if err != nil {
			return nil, fmt.Errorf("tar walk: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name != "control" {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("tar read body: %w", err)
		}
		return body, nil
	}
}

// ExtractControl is the end-to-end helper: decompress + tar-walk on already-
// buffered control.tar bytes.
func ExtractControl(memberName string, controlTar []byte) ([]byte, error) {
	r, err := DecompressControlTar(memberName, controlTar)
	if err != nil {
		return nil, err
	}
	return ReadControlFromTar(r)
}
