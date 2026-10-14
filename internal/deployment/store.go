package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func Load(path string) (State, error) {
	file, err := os.Open(path)
	if err != nil {
		return State{}, fmt.Errorf("open deployment state %q: %w", path, err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("decode deployment state %q: %w", path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return State{}, fmt.Errorf("decode deployment state %q: %w", path, err)
	}
	if err := state.Validate(); err != nil {
		return State{}, fmt.Errorf("validate deployment state %q: %w", path, err)
	}
	return state, nil
}

func Save(path string, state State) error {
	if err := state.Validate(); err != nil {
		return fmt.Errorf("refuse to persist invalid deployment state: %w", err)
	}
	if path == "" {
		return errors.New("deployment state path must not be empty")
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode deployment state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("inspect deployment state directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("deployment state directory %q is not a directory", dir)
	}

	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary deployment state: %w", err)
	}
	tempPath := temp.Name()
	cleanup := func() {
		temp.Close()
		os.Remove(tempPath)
	}
	defer cleanup()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary deployment state permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write temporary deployment state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary deployment state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary deployment state: %w", err)
	}
	if err := replaceFile(tempPath, path); err != nil {
		return fmt.Errorf("atomically replace deployment state %q: %w", path, err)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected trailing JSON value")
	}
	return err
}

func Marshal(state State) ([]byte, error) {
	if err := state.Validate(); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
