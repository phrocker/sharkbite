package embeddingsidecar

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const maxKeyringBytes = 64 << 10
const keyringDomain = "sharkbite.embedding-sidecar.keyring.v1\x00"

// HMACKeyring signs new markers with one active key and verifies old markers
// with retained keys. A keyring is immutable after loading; rotation replaces
// the secret file and restarts the process with the old key still present.
type HMACKeyring struct {
	namespace string
	current   string
	keys      map[string][]byte
}

var _ MarkerAuthenticator = (*HMACKeyring)(nil)

type keyringDocument struct {
	Version   int    `json:"version"`
	Namespace string `json:"namespace"`
	Current   string `json:"current"`
	Keys      []struct {
		ID             string `json:"id"`
		MaterialBase64 string `json:"material_base64"`
	} `json:"keys"`
}

// LoadHMACKeyring reads a bounded, owner-only secret file. The namespace must
// equal the configured Accumulo authority namespace; using the same raw key
// under another namespace produces a different MAC. Key IDs are stable across
// rotation and removed only after all records signed by them are retired.
func LoadHMACKeyring(filename, namespace string) (*HMACKeyring, error) {
	if filename == "" || !validKeyNamespace(namespace) {
		return nil, ErrInvalid
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 ||
		info.Size() <= 0 || info.Size() > maxKeyringBytes {
		return nil, fmt.Errorf("%w: keyring must be a bounded owner-only regular file", ErrInvalid)
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maxKeyringBytes+1))
	if err != nil {
		return nil, err
	}
	if len(encoded) == 0 || len(encoded) > maxKeyringBytes {
		return nil, ErrInvalid
	}
	uniqueness := json.NewDecoder(bytes.NewReader(encoded))
	if rejectDuplicateJSONFields(uniqueness, 0) != nil || uniqueness.Decode(new(any)) != io.EOF {
		return nil, ErrInvalid
	}
	var document keyringDocument
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF ||
		document.Version != Version || document.Namespace != namespace ||
		!validKeyID(document.Current) || len(document.Keys) == 0 || len(document.Keys) > 32 {
		return nil, ErrInvalid
	}
	keys := make(map[string][]byte, len(document.Keys))
	for _, item := range document.Keys {
		if !validKeyID(item.ID) || item.MaterialBase64 == "" {
			return nil, ErrInvalid
		}
		if _, exists := keys[item.ID]; exists {
			return nil, ErrInvalid
		}
		material, err := base64.StdEncoding.Strict().DecodeString(item.MaterialBase64)
		if err != nil || len(material) < 32 || len(material) > 128 {
			return nil, ErrInvalid
		}
		keys[item.ID] = material
	}
	if _, exists := keys[document.Current]; !exists {
		return nil, ErrInvalid
	}
	return &HMACKeyring{namespace: namespace, current: document.Current, keys: keys}, nil
}

func rejectDuplicateJSONFields(decoder *json.Decoder, depth int) error {
	if depth > 16 {
		return ErrInvalid
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			field, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := field.(string)
			if !ok {
				return ErrInvalid
			}
			if _, duplicate := seen[name]; duplicate {
				return ErrInvalid
			}
			seen[name] = struct{}{}
			if err := rejectDuplicateJSONFields(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := rejectDuplicateJSONFields(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return ErrInvalid
	}
}

func validKeyNamespace(namespace string) bool {
	return namespace != "" && len(namespace) <= 128 && utf8.ValidString(namespace) &&
		!strings.ContainsRune(namespace, 0)
}

func validKeyID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
			return false
		}
	}
	return true
}

func (k *HMACKeyring) KeyID(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if k == nil {
		return "", ErrInvalid
	}
	return k.current, nil
}

func (k *HMACKeyring) Sign(ctx context.Context, keyID string, payload []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if k == nil || keyID != k.current {
		return "", ErrInvalid
	}
	key, exists := k.keys[keyID]
	if !exists {
		return "", ErrInvalid
	}
	return hex.EncodeToString(k.mac(key, payload)), nil
}

func (k *HMACKeyring) Verify(
	ctx context.Context, keyID string, payload []byte, signature string,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if k == nil {
		return false, ErrInvalid
	}
	key, exists := k.keys[keyID]
	if !exists || !validDigest(signature) {
		return false, nil
	}
	actual, _ := hex.DecodeString(signature)
	return hmac.Equal(k.mac(key, payload), actual), nil
}

func (k *HMACKeyring) mac(key, payload []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(keyringDomain))
	_, _ = mac.Write([]byte(k.namespace))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}
