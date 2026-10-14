package bcfile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// FooterMinSizeV3 is the smallest possible BCFile size for a v3 file: the
// magic + version + 2 trailer longs (offsetIndexMeta + offsetCryptoParams).
// Used for sanity checks before seeking into the trailer.
const FooterMinSizeV3 = MagicSize + VersionSize + 16

// FooterMinSizeV1 is the smallest possible BCFile size for a v1 file:
// magic + version + 1 trailer long (offsetIndexMeta).
const FooterMinSizeV1 = MagicSize + VersionSize + 8

// Footer carries everything we need from the trailer of a BCFile: the
// version stamp and the offsets it points to.
type Footer struct {
	Version            Version
	OffsetIndexMeta    int64
	OffsetCryptoParams int64 // 0 for v1 (the trailer doesn't carry it)
}

// ErrFileTooShort indicates the file is smaller than the smallest possible
// BCFile trailer.
var ErrFileTooShort = errors.New("bcfile: file too short for a valid BCFile trailer")

// ErrInvalidFooterOffsets indicates that trailer offsets are negative,
// non-monotonic, or outside the file body.
var ErrInvalidFooterOffsets = errors.New("bcfile: invalid footer offsets")

// ReadFooter scans the trailer of a BCFile by reading backwards from the
// end of the file (offset fileLength - magicSize - versionSize). Mirrors
// the Java BCFile.Reader constructor's preamble.
//
// Layout (final bytes, in file-offset order):
//
//	[..., offsetIndexMeta (8B), offsetCryptoParams (8B, v3 only), version (4B), magic (16B)]
//
// fileLength must be the exact byte length of the BCFile in r. r must
// support random access via io.ReaderAt — typical sources: os.File,
// *bytes.Reader (in-memory tests), or a GCS object reader.
func ReadFooter(r io.ReaderAt, fileLength int64) (Footer, error) {
	if fileLength < int64(FooterMinSizeV1) {
		return Footer{}, fmt.Errorf("%w: have %d bytes, need at least %d",
			ErrFileTooShort, fileLength, FooterMinSizeV1)
	}

	// Step 1: read version + magic at the very end.
	tailStart := fileLength - int64(MagicSize) - int64(VersionSize)
	tail := make([]byte, MagicSize+VersionSize)
	if _, err := r.ReadAt(tail, tailStart); err != nil {
		return Footer{}, fmt.Errorf("bcfile: read trailer (version+magic): %w", err)
	}
	versionBytes := tail[0:VersionSize]
	magicBytes := tail[VersionSize:]

	if !bytes.Equal(magicBytes, MagicBytes[:]) {
		return Footer{}, ErrBadMagic
	}
	version := Version{
		Major: int16(binary.BigEndian.Uint16(versionBytes[0:2])),
		Minor: int16(binary.BigEndian.Uint16(versionBytes[2:4])),
	}
	if err := CheckSupported(version); err != nil {
		return Footer{}, err
	}

	// Step 2: read the offset(s). v3 has both offsetIndexMeta and
	// offsetCryptoParams; v1 has only offsetIndexMeta.
	switch {
	case version.CompatibleWith(APIVersion3):
		if fileLength < int64(FooterMinSizeV3) {
			return Footer{}, fmt.Errorf("%w: v3 file too short, have %d need %d",
				ErrFileTooShort, fileLength, FooterMinSizeV3)
		}
		offStart := tailStart - 16
		var offBuf [16]byte
		if _, err := r.ReadAt(offBuf[:], offStart); err != nil {
			return Footer{}, fmt.Errorf("bcfile: read v3 offsets: %w", err)
		}
		footer := Footer{
			Version:            version,
			OffsetIndexMeta:    int64(binary.BigEndian.Uint64(offBuf[0:8])),
			OffsetCryptoParams: int64(binary.BigEndian.Uint64(offBuf[8:16])),
		}
		if err := validateFooterOffsets(footer, fileLength); err != nil {
			return Footer{}, err
		}
		return footer, nil

	case version.CompatibleWith(APIVersion1):
		offStart := tailStart - 8
		var offBuf [8]byte
		if _, err := r.ReadAt(offBuf[:], offStart); err != nil {
			return Footer{}, fmt.Errorf("bcfile: read v1 offset: %w", err)
		}
		footer := Footer{
			Version:         version,
			OffsetIndexMeta: int64(binary.BigEndian.Uint64(offBuf[:])),
		}
		if err := validateFooterOffsets(footer, fileLength); err != nil {
			return Footer{}, err
		}
		return footer, nil

	default:
		// CheckSupported guards above — but be defensive.
		return Footer{}, fmt.Errorf("%w: unhandled version %s", ErrUnsupportedVersion, version)
	}
}

func validateFooterOffsets(footer Footer, fileLength int64) error {
	if footer.OffsetIndexMeta < 0 {
		return fmt.Errorf("%w: MetaIndex offset %d is negative",
			ErrInvalidFooterOffsets, footer.OffsetIndexMeta)
	}
	if footer.Version.CompatibleWith(APIVersion3) {
		trailerStart := fileLength - int64(FooterMinSizeV3)
		if footer.OffsetCryptoParams < 0 {
			return fmt.Errorf("%w: crypto params offset %d is negative",
				ErrInvalidFooterOffsets, footer.OffsetCryptoParams)
		}
		if footer.OffsetIndexMeta >= footer.OffsetCryptoParams {
			return fmt.Errorf("%w: MetaIndex offset %d must precede crypto params offset %d",
				ErrInvalidFooterOffsets, footer.OffsetIndexMeta, footer.OffsetCryptoParams)
		}
		if footer.OffsetCryptoParams > trailerStart {
			return fmt.Errorf("%w: crypto params offset %d exceeds trailer start %d",
				ErrInvalidFooterOffsets, footer.OffsetCryptoParams, trailerStart)
		}
		return nil
	}

	trailerStart := fileLength - int64(FooterMinSizeV1)
	if footer.OffsetIndexMeta >= trailerStart {
		return fmt.Errorf("%w: MetaIndex offset %d must precede trailer start %d",
			ErrInvalidFooterOffsets, footer.OffsetIndexMeta, trailerStart)
	}
	return nil
}

// WriteFooter writes the BCFile trailer to w in v3 form. Used by tests that
// build synthetic BCFiles. v1 callers should write their own 8-byte
// trailer; we don't expose a v1 writer because there's no reason to
// produce v1 files in this codebase (we only ever read them).
func WriteFooter(w io.Writer, f Footer) error {
	if !f.Version.CompatibleWith(APIVersion3) {
		return fmt.Errorf("WriteFooter only supports v3 (got %s)", f.Version)
	}
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[0:8], uint64(f.OffsetIndexMeta))
	binary.BigEndian.PutUint64(buf[8:16], uint64(f.OffsetCryptoParams))
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}
	if err := WriteVersion(w, f.Version); err != nil {
		return err
	}
	return WriteMagic(w)
}
