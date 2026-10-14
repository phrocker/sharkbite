package accumulo

import (
	"errors"
	"sync"
)

const PythonIteratorClass = "org.poma.accumulo.JythonIterator"

// NewIterInfo constructs the option-free iterator descriptor exposed by
// Sharkbite's IterInfo(name, class, priority) constructor.
func NewIterInfo(name, className string, priority int32) (IteratorSetting, error) {
	return NewIteratorSetting(name, className, priority, nil)
}

// PythonIterator is the client-side descriptor for Sharkbite's PythonIterator.
// Shoal preserves descriptor semantics without claiming server-side Python
// execution. Getters may be called concurrently; OnNext must not race another
// OnNext call when callers require deterministic last-writer ordering.
type PythonIterator struct {
	mu       sync.RWMutex
	name     string
	script   string
	priority int32
}

// NewPythonIterator constructs the two-argument form, ready for OnNext.
func NewPythonIterator(name string, priority int32) (*PythonIterator, error) {
	return newPythonIterator(name, "", priority)
}

// NewPythonIteratorScript constructs the script-carrying three-argument form.
func NewPythonIteratorScript(name, script string, priority int32) (*PythonIterator, error) {
	if script == "" {
		return nil, errors.New("accumulo: Python iterator script must be non-empty")
	}
	return newPythonIterator(name, script, priority)
}

func newPythonIterator(name, script string, priority int32) (*PythonIterator, error) {
	if _, err := NewIterInfo(name, PythonIteratorClass, priority); err != nil {
		return nil, err
	}
	return &PythonIterator{name: name, script: script, priority: priority}, nil
}

// OnNext installs a lambda source and returns the same descriptor for chaining.
func (p *PythonIterator) OnNext(source string) (*PythonIterator, error) {
	if p == nil {
		return nil, errors.New("accumulo: Python iterator is nil")
	}
	if source == "" {
		return nil, errors.New("accumulo: Python iterator lambda must be non-empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.script != "" {
		return nil, errors.New("accumulo: cannot provide OnNext when a Python script is provided")
	}
	p.script = source
	return p, nil
}

func (p *PythonIterator) Name() string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.name
}

func (p *PythonIterator) ClassName() string { return PythonIteratorClass }

func (p *PythonIterator) Priority() int32 {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.priority
}

func (p *PythonIterator) Script() string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.script
}

// IteratorSetting snapshots the descriptor into the existing scanner setting.
func (p *PythonIterator) IteratorSetting() (IteratorSetting, error) {
	if p == nil {
		return IteratorSetting{}, errors.New("accumulo: Python iterator is nil")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	options := map[string]string{}
	if p.script != "" {
		options["python.script"] = p.script
	}
	return NewIteratorSetting(p.name, PythonIteratorClass, p.priority, options)
}
