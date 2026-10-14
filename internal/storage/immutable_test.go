package storage

import (
	"errors"
	"testing"
)

func TestImmutableDescriptorPurposeMatrix(t *testing.T) {
	tests := []struct {
		name       string
		descriptor ImmutableDescriptor
		purpose    ImmutablePurpose
		wantOK     bool
	}{
		{
			name:       "authoritative RFile is local",
			descriptor: ImmutableDescriptor{Format: ImmutableFormatRFile, Role: ImmutableRoleAuthoritative},
			purpose:    ImmutablePurposeLocalStorage,
			wantOK:     true,
		},
		{
			name:       "authoritative Parquet is local",
			descriptor: ImmutableDescriptor{Format: ImmutableFormatParquet, Role: ImmutableRoleAuthoritative},
			purpose:    ImmutablePurposeLocalStorage,
			wantOK:     true,
		},
		{
			name:       "authoritative RFile is promotable",
			descriptor: ImmutableDescriptor{Format: ImmutableFormatRFile, Role: ImmutableRoleAuthoritative},
			purpose:    ImmutablePurposeAccumuloPromotion,
			wantOK:     true,
		},
		{
			name:       "authoritative RFile is importable",
			descriptor: ImmutableDescriptor{Format: ImmutableFormatRFile, Role: ImmutableRoleAuthoritative},
			purpose:    ImmutablePurposeRFileImport,
			wantOK:     true,
		},
		{
			name:       "authoritative Parquet is not RFile importable",
			descriptor: ImmutableDescriptor{Format: ImmutableFormatParquet, Role: ImmutableRoleAuthoritative},
			purpose:    ImmutablePurposeRFileImport,
		},
		{
			name:       "authoritative Parquet is not promotable",
			descriptor: ImmutableDescriptor{Format: ImmutableFormatParquet, Role: ImmutableRoleAuthoritative},
			purpose:    ImmutablePurposeAccumuloPromotion,
		},
		{
			name:       "derived RFile is not promotable",
			descriptor: ImmutableDescriptor{Format: ImmutableFormatRFile, Role: ImmutableRoleDerived},
			purpose:    ImmutablePurposeAccumuloPromotion,
		},
		{
			name:       "derived Parquet is not authoritative local storage",
			descriptor: ImmutableDescriptor{Format: ImmutableFormatParquet, Role: ImmutableRoleDerived},
			purpose:    ImmutablePurposeLocalStorage,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.descriptor.ValidateFor(test.purpose)
			if test.wantOK {
				if err != nil {
					t.Fatalf("ValidateFor() error = %v", err)
				}
				return
			}
			if !errors.Is(err, ErrImmutablePolicy) {
				t.Fatalf("ValidateFor() error = %v, want ErrImmutablePolicy", err)
			}
		})
	}
}

func TestImmutableFormatForPath(t *testing.T) {
	tests := map[string]ImmutableFormat{
		"tablet/F0001.rf":      ImmutableFormatRFile,
		"tablet/F0001.parquet": ImmutableFormatParquet,
		"tablet/files.json":    "",
		"tablet/F0001.RF":      "",
	}
	for path, want := range tests {
		if got := ImmutableFormatForPath(path); got != want {
			t.Errorf("ImmutableFormatForPath(%q) = %q, want %q", path, got, want)
		}
	}
}
