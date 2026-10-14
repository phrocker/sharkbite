package storage

import (
	"errors"
	"fmt"
	"path/filepath"
)

// ImmutableFormat identifies the physical encoding of a local immutable file.
type ImmutableFormat string

const (
	ImmutableFormatRFile   ImmutableFormat = "rfile"
	ImmutableFormatParquet ImmutableFormat = "parquet"
)

// ImmutableRole identifies whether an immutable file is part of the
// authoritative local tablet generation or is a derived artifact.
type ImmutableRole string

const (
	ImmutableRoleAuthoritative ImmutableRole = "authoritative"
	ImmutableRoleDerived       ImmutableRole = "derived"
)

// ImmutablePurpose is the operation an immutable file must be safe to serve.
type ImmutablePurpose string

const (
	ImmutablePurposeLocalStorage      ImmutablePurpose = "local-storage"
	ImmutablePurposeRFileImport       ImmutablePurpose = "rfile-import"
	ImmutablePurposeAccumuloPromotion ImmutablePurpose = "accumulo-promotion"
)

// ErrImmutablePolicy is returned when an immutable file's format and role do
// not satisfy the requested purpose.
var ErrImmutablePolicy = errors.New("storage: immutable file policy violation")

// ImmutableDescriptor is the typed format/role policy carried by a local
// immutable file. It deliberately stays out of persisted manifests.
type ImmutableDescriptor struct {
	Format ImmutableFormat
	Role   ImmutableRole
}

// ImmutableFormatForPath derives the physical format from the immutable file
// name used by local tablet storage.
func ImmutableFormatForPath(path string) ImmutableFormat {
	switch filepath.Ext(path) {
	case ".rf":
		return ImmutableFormatRFile
	case ".parquet":
		return ImmutableFormatParquet
	default:
		return ""
	}
}

// ValidateFor verifies the format/role capability matrix. Both RFile and
// Parquet may be authoritative local immutable files, while Accumulo bulk
// promotion is intentionally restricted to authoritative RFiles.
func (d ImmutableDescriptor) ValidateFor(purpose ImmutablePurpose) error {
	if d.Format != ImmutableFormatRFile && d.Format != ImmutableFormatParquet {
		return fmt.Errorf("%w: unsupported format %q", ErrImmutablePolicy, d.Format)
	}
	if d.Role != ImmutableRoleAuthoritative && d.Role != ImmutableRoleDerived {
		return fmt.Errorf("%w: unsupported role %q", ErrImmutablePolicy, d.Role)
	}

	switch purpose {
	case ImmutablePurposeLocalStorage:
		if d.Role == ImmutableRoleAuthoritative {
			return nil
		}
	case ImmutablePurposeRFileImport, ImmutablePurposeAccumuloPromotion:
		if d.Format == ImmutableFormatRFile && d.Role == ImmutableRoleAuthoritative {
			return nil
		}
	default:
		return fmt.Errorf("%w: unsupported purpose %q", ErrImmutablePolicy, purpose)
	}

	return fmt.Errorf("%w: format %q with role %q cannot serve purpose %q",
		ErrImmutablePolicy, d.Format, d.Role, purpose)
}

// ValidateImmutablePath derives path's format and verifies its role for
// purpose.
func ValidateImmutablePath(path string, role ImmutableRole, purpose ImmutablePurpose) error {
	return (ImmutableDescriptor{
		Format: ImmutableFormatForPath(path),
		Role:   role,
	}).ValidateFor(purpose)
}
