package accumulo

import (
	"sync"
	"testing"
)

func TestPythonIteratorDescriptorConstructorsAndSnapshot(t *testing.T) {
	iterator, err := NewPythonIterator("filter", 7)
	if err != nil {
		t.Fatal(err)
	}
	chained, err := iterator.OnNext("lambda key, value: True")
	if err != nil {
		t.Fatal(err)
	}
	if chained != iterator {
		t.Fatal("OnNext did not return the same descriptor")
	}
	if iterator.Name() != "filter" || iterator.Priority() != 7 ||
		iterator.ClassName() != PythonIteratorClass {
		t.Fatalf("unexpected descriptor: %#v", iterator)
	}
	setting, err := iterator.IteratorSetting()
	if err != nil {
		t.Fatal(err)
	}
	if got := setting.Options()["python.script"]; got != "lambda key, value: True" {
		t.Fatalf("script = %q", got)
	}

	script, err := NewPythonIteratorScript("script", "class script: pass", 9)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := script.OnNext("lambda value: value"); err == nil {
		t.Fatal("OnNext accepted a script-carrying descriptor")
	}
}

func TestPythonIteratorConcurrentGetters(t *testing.T) {
	iterator, err := NewPythonIteratorScript("script", "class script: pass", 9)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if iterator.Name() != "script" || iterator.Priority() != 9 ||
				iterator.Script() != "class script: pass" {
				t.Error("descriptor changed during concurrent read")
			}
			if _, err := iterator.IteratorSetting(); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
}
