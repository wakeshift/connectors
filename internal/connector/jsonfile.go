package connector

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Editing another program's JSON settings: keep everything as written (key
// order and all) apart from the entries we add or remove, back the file up
// before changing it, refuse a file that isn't valid JSON (rather than
// guess), and write a linked file (dotfiles) through its link, keeping its
// permissions.

// Object is a JSON object that keeps its keys in order.
type Object struct {
	keys []string
	vals map[string]json.RawMessage
}

// ParseObject parses a JSON object, keeping its key order.
func ParseObject(b []byte) (*Object, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	o := &Object{vals: map[string]json.RawMessage{}}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		k, _ := t.(string)
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return nil, err
		}
		if _, dup := o.vals[k]; !dup {
			o.keys = append(o.keys, k)
		}
		o.vals[k] = v
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err == nil {
		return nil, errors.New("more than one JSON value")
	}
	return o, nil
}

// Get returns a key's value.
func (o *Object) Get(k string) (json.RawMessage, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// Set sets a key (new keys go last).
func (o *Object) Set(k string, v json.RawMessage) {
	if o.vals == nil {
		o.vals = map[string]json.RawMessage{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// Delete removes a key.
func (o *Object) Delete(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, kk := range o.keys {
		if kk == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Len is the number of keys.
func (o *Object) Len() int { return len(o.keys) }

// Child returns the object at k (a new, empty one if there's none, or an
// error if k holds something else).
func (o *Object) Child(k string) (*Object, error) {
	v, ok := o.Get(k)
	if !ok || string(bytes.TrimSpace(v)) == "null" {
		return &Object{}, nil
	}
	c, err := ParseObject(v)
	if err != nil {
		return nil, fmt.Errorf("%q isn't an object", k)
	}
	return c, nil
}

// List returns the array at k (empty if there's none).
func (o *Object) List(k string) ([]json.RawMessage, error) {
	v, ok := o.Get(k)
	if !ok || string(bytes.TrimSpace(v)) == "null" {
		return nil, nil
	}
	var l []json.RawMessage
	if err := json.Unmarshal(v, &l); err != nil {
		return nil, fmt.Errorf("%q isn't a list", k)
	}
	return l, nil
}

// SetList sets k to a list.
func (o *Object) SetList(k string, l []json.RawMessage) {
	if l == nil {
		l = []json.RawMessage{}
	}
	b, _ := json.Marshal(l)
	o.Set(k, b)
}

// Marshal is the object as JSON, in its order.
func (o *Object) Marshal() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// ReadJSONFile reads a settings file as an object (empty if there's no
// file).
func ReadJSONFile(path string) (*Object, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Object{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return &Object{}, nil
	}
	o, err := ParseObject(b)
	if err != nil {
		return nil, fmt.Errorf("%s isn't valid JSON (comments aren't supported here); fix or move it, then run this again: %w", path, err)
	}
	return o, nil
}

// WriteJSONFile writes an object to a settings file, indented, through a
// link if the file is one, keeping its permissions (0600 for a new file).
func WriteJSONFile(path string, o *Object) error {
	var b bytes.Buffer
	if err := json.Indent(&b, o.Marshal(), "", "  "); err != nil {
		return err
	}
	b.WriteByte('\n')
	mode := os.FileMode(0o600)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".wakeshift-tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Backup copies a file next to itself before it's changed ("" if there was
// no file).
func Backup(path string) (string, error) {
	old, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Never over an earlier backup (an install and an uninstall in one
	// second).
	base := path + ".before-wakeshift-" + time.Now().Format("20060102-150405")
	for n := 1; ; n++ {
		backup := base
		if n > 1 {
			backup = fmt.Sprintf("%s-%d", base, n)
		}
		f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) && n < 100 {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(old)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return backup, err
	}
}
