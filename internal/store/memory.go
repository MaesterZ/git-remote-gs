package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// Memory is an in-memory Store with GCS-like generations and preconditions, for tests.
type Memory struct {
	mu      sync.Mutex
	objects map[string]memObject
	lastGen int64
	now     func() time.Time
}

type memObject struct {
	data []byte
	attr Object
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{objects: map[string]memObject{}, now: time.Now}
}

// List implements Store.
func (m *Memory) List(ctx context.Context, prefix string) ([]Object, error) {
	err := ctx.Err()
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Object

	for name, obj := range m.objects {
		if strings.HasPrefix(name, prefix) {
			out = append(out, obj.attr)
		}
	}

	slices.SortFunc(out, func(a, b Object) int { return strings.Compare(a.Name, b.Name) })

	return out, nil
}

// Read implements Store.
func (m *Memory) Read(ctx context.Context, name string, w io.Writer) (Object, error) {
	err := ctx.Err()
	if err != nil {
		return Object{}, fmt.Errorf("read %s: %w", name, err)
	}

	m.mu.Lock()
	obj, ok := m.objects[name]
	m.mu.Unlock()

	if !ok {
		return Object{}, fmt.Errorf("read %s: %w", name, ErrNotFound)
	}

	_, err = w.Write(obj.data)
	if err != nil {
		return Object{}, fmt.Errorf("read %s: %w", name, err)
	}

	return obj.attr, nil
}

// WriteIfAbsent implements Store.
func (m *Memory) WriteIfAbsent(ctx context.Context, name string, r io.Reader, _ string) (Object, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Object{}, fmt.Errorf("write %s: %w", name, err)
	}

	err = ctx.Err()
	if err != nil {
		return Object{}, fmt.Errorf("write %s: %w", name, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.objects[name]; ok {
		return Object{}, fmt.Errorf("write %s: %w", name, ErrPrecondition)
	}

	m.lastGen++
	attr := Object{Name: name, Generation: m.lastGen, Created: m.now(), Size: int64(len(data))}
	m.objects[name] = memObject{data: data, attr: attr}

	return attr, nil
}

// ReplaceIfGeneration implements Store.
func (m *Memory) ReplaceIfGeneration(ctx context.Context, name string, generation int64, r io.Reader, _ string) (Object, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Object{}, fmt.Errorf("replace %s: %w", name, err)
	}

	err = ctx.Err()
	if err != nil {
		return Object{}, fmt.Errorf("replace %s: %w", name, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Like GCS, a generation precondition on a missing object fails as a precondition, not as not-found.
	if obj, ok := m.objects[name]; !ok || obj.attr.Generation != generation {
		return Object{}, fmt.Errorf("replace %s: generation %d: %w", name, generation, ErrPrecondition)
	}

	m.lastGen++
	attr := Object{Name: name, Generation: m.lastGen, Created: m.now(), Size: int64(len(data))}
	m.objects[name] = memObject{data: data, attr: attr}

	return attr, nil
}

// ReadRange implements Store.
func (m *Memory) ReadRange(ctx context.Context, name string, generation, offset, length int64, w io.Writer) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}

	m.mu.Lock()
	obj, ok := m.objects[name]
	m.mu.Unlock()

	if !ok || obj.attr.Generation != generation {
		return fmt.Errorf("read %s generation %d: %w", name, generation, ErrNotFound)
	}

	if offset < 0 || length < 0 || offset+length > int64(len(obj.data)) {
		return fmt.Errorf("read %s at %d: %d of %d bytes: %w", name, offset, length, len(obj.data), ErrShortRead)
	}

	_, err = w.Write(obj.data[offset : offset+length])
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}

	return nil
}

// DeleteIfGeneration implements Store.
func (m *Memory) DeleteIfGeneration(ctx context.Context, name string, generation int64) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("delete %s: %w", name, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	obj, ok := m.objects[name]

	switch {
	case !ok:
		return fmt.Errorf("delete %s: %w", name, ErrNotFound)
	case obj.attr.Generation != generation:
		return fmt.Errorf("delete %s: generation %d, want %d: %w", name, obj.attr.Generation, generation, ErrPrecondition)
	}

	delete(m.objects, name)

	return nil
}

// Put writes or replaces an object unconditionally, backdating it to created when non-zero; for test setup only.
func (m *Memory) Put(name string, data []byte, created time.Time) Object {
	m.mu.Lock()
	defer m.mu.Unlock()

	if created.IsZero() {
		created = m.now()
	}

	m.lastGen++
	attr := Object{Name: name, Generation: m.lastGen, Created: created, Size: int64(len(data))}
	m.objects[name] = memObject{data: bytes.Clone(data), attr: attr}

	return attr
}

// SetClock sets the time new objects are stamped with, for tests that fake the clock.
func (m *Memory) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.now = now
}

// Get returns an object's content, for test assertions.
func (m *Memory) Get(name string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	obj, ok := m.objects[name]

	return bytes.Clone(obj.data), ok
}

// Names returns all object names, sorted, for test assertions.
func (m *Memory) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := make([]string, 0, len(m.objects))
	for name := range m.objects {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}
