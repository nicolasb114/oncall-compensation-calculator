// Package store keeps connections (token in the OS credential store) and saved reports on disk.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

const keyringService = "oncall-compensation-calculator"

// Connection is a saved PagerDuty account (the API key itself lives in the credential store).
type Connection struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Region    string    `json:"region"` // "us" or "eu"
	KeyTail   string    `json:"keyTail"`
	Analytics bool      `json:"analytics"`
	Saved     time.Time `json:"saved"`
}

// SavedReport is a reusable report setup: everything except the dates is kept.
type SavedReport struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	ConnID  string          `json:"connId"`
	Config  json.RawMessage `json:"config"`
	Updated time.Time       `json:"updated"`
	LastRun string          `json:"lastRun,omitempty"`
}

// Store is the on-disk state.
type Store struct {
	Dir string
	mu  sync.Mutex
	// Secrets is overridable for tests.
	Secrets SecretStore
}

// SecretStore abstracts the OS credential store.
type SecretStore interface {
	Set(id, secret string) error
	Get(id string) (string, error)
	Delete(id string) error
}

type osSecrets struct{}

func (osSecrets) Set(id, s string) error        { return keyring.Set(keyringService, id, s) }
func (osSecrets) Get(id string) (string, error) { return keyring.Get(keyringService, id) }
func (osSecrets) Delete(id string) error        { return keyring.Delete(keyringService, id) }

type memSecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memSecrets) Set(id, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = v
	return nil
}
func (s *memSecrets) Get(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[id]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}
func (s *memSecrets) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	return nil
}

// Open uses the user's config directory.
func Open() (*Store, error) {
	if d := os.Getenv("ONCALL_TEST_DIR"); d != "" {
		return OpenAt(d)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return OpenAt(filepath.Join(base, "oncall-compensation-calculator"))
}

func OpenAt(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st := &Store{Dir: dir, Secrets: osSecrets{}}
	if os.Getenv("ONCALL_TEST_MODE") == "1" { // never touches the real credential store
		st.Secrets = &memSecrets{m: map[string]string{}}
	}
	return st, nil
}

func (s *Store) read(name string, v any) error {
	b, err := os.ReadFile(filepath.Join(s.Dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (s *Store) write(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, name))
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func newID(name string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-") + "-" + fmt.Sprint(time.Now().UnixNano()%1e6)
}

// Connections lists saved connections.
func (s *Store) Connections() ([]Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c []Connection
	err := s.read("connections.json", &c)
	return c, err
}

// AddConnection saves the token in the credential store and the metadata on disk.
func (s *Store) AddConnection(name, region, token string, analytics bool) (Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []Connection
	if err := s.read("connections.json", &all); err != nil {
		return Connection{}, err
	}
	tail := token
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	c := Connection{ID: newID(name), Name: name, Region: region, KeyTail: tail, Analytics: analytics, Saved: time.Now()}
	if err := s.Secrets.Set(c.ID, token); err != nil {
		return Connection{}, fmt.Errorf("could not store the key in the OS credential store: %w", err)
	}
	all = append(all, c)
	return c, s.write("connections.json", all)
}

// Token returns the stored API key.
func (s *Store) Token(id string) (string, error) { return s.Secrets.Get(id) }

// DeleteConnection forgets a connection and its key.
func (s *Store) DeleteConnection(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all, keep []Connection
	if err := s.read("connections.json", &all); err != nil {
		return err
	}
	for _, c := range all {
		if c.ID != id {
			keep = append(keep, c)
		}
	}
	_ = s.Secrets.Delete(id)
	return s.write("connections.json", keep)
}

// Reports lists saved reports.
func (s *Store) Reports() ([]SavedReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r []SavedReport
	err := s.read("reports.json", &r)
	sort.Slice(r, func(i, j int) bool { return r[i].Updated.After(r[j].Updated) })
	return r, err
}

// SaveReport inserts or replaces a saved report.
func (s *Store) SaveReport(r SavedReport) (SavedReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []SavedReport
	if err := s.read("reports.json", &all); err != nil {
		return r, err
	}
	if r.Name == "" {
		return r, errors.New("a saved report needs a name")
	}
	r.Updated = time.Now()
	if r.ID == "" {
		r.ID = newID(r.Name)
		all = append(all, r)
	} else {
		found := false
		for i := range all {
			if all[i].ID == r.ID {
				all[i] = r
				found = true
			}
		}
		if !found {
			all = append(all, r)
		}
	}
	return r, s.write("reports.json", all)
}

// DeleteReport removes a saved report.
func (s *Store) DeleteReport(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all, keep []SavedReport
	if err := s.read("reports.json", &all); err != nil {
		return err
	}
	for _, r := range all {
		if r.ID != id {
			keep = append(keep, r)
		}
	}
	return s.write("reports.json", keep)
}

// Report returns one saved report.
func (s *Store) Report(id string) (SavedReport, bool) {
	rs, _ := s.Reports()
	for _, r := range rs {
		if r.ID == id {
			return r, true
		}
	}
	return SavedReport{}, false
}
