package store

import (
	"encoding/json"
	"errors"
	"testing"
)

type mem map[string]string

func (m mem) Set(id, s string) error { m[id] = s; return nil }
func (m mem) Get(id string) (string, error) {
	if v, ok := m[id]; ok {
		return v, nil
	}
	return "", errors.New("missing")
}
func (m mem) Delete(id string) error { delete(m, id); return nil }

func TestConnectionsAndReports(t *testing.T) {
	s, _ := OpenAt(t.TempDir())
	s.Secrets = mem{}
	c, err := s.AddConnection("Acme", "us", "secret-token-1234", true)
	if err != nil || c.KeyTail != "1234" {
		t.Fatal(err, c)
	}
	if tok, _ := s.Token(c.ID); tok != "secret-token-1234" {
		t.Fatal("token not stored")
	}
	// The token must never be written to disk in plain text.
	cs, _ := s.Connections()
	b, _ := json.Marshal(cs)
	if string(b) == "" || contains(string(b), "secret-token") {
		t.Fatal("token leaked to disk")
	}
	r, err := s.SaveReport(SavedReport{Name: "Monthly", ConnID: c.ID, Config: json.RawMessage(`{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	r.Name = "Monthly v2"
	s.SaveReport(r)
	rs, _ := s.Reports()
	if len(rs) != 1 || rs[0].Name != "Monthly v2" {
		t.Fatalf("%v", rs)
	}
	s.DeleteReport(r.ID)
	s.DeleteConnection(c.ID)
	if rs, _ := s.Reports(); len(rs) != 0 {
		t.Fatal("not deleted")
	}
	if _, err := s.Token(c.ID); err == nil {
		t.Fatal("token not removed")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
