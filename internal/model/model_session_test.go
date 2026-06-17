package model

import (
	"testing"
)

func TestModelSessionRemoveCallback(t *testing.T) {
	m := NewModelConnection()
	s := NewModelSession("test1", m)

	s.AddCallback(func(event CallbackEvent) {})

	// RemoveCallback is simplified for now - removes all callbacks
	s.RemoveCallback(func(event CallbackEvent) {})

	if len(s.Callbacks) != 0 {
		t.Errorf("Expected 0 callbacks after removal, got %d", len(s.Callbacks))
	}
}
