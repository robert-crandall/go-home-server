package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/robert-crandall/go-home-server/apiclient"
)

func TestListNotes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/notes" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("missing/wrong auth header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":1,"body":"hello","createdAt":"2024-01-01T00:00:00Z"}]`)
	}))
	defer srv.Close()

	c := apiclient.New(srv.URL, "test-token")
	out, err := listNotes(context.Background(), c, listNotesIn{})
	if err != nil {
		t.Fatalf("listNotes: %v", err)
	}
	if len(out.Notes) != 1 || out.Notes[0].Body != "hello" {
		t.Errorf("unexpected notes: %+v", out.Notes)
	}
}

func TestCreateNote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/notes" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			http.Error(w, "decode error", http.StatusBadRequest)
			return
		}
		if body["body"] != "new note" {
			t.Errorf("unexpected request body: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":7,"body":"new note","createdAt":"2024-01-02T00:00:00Z"}`)
	}))
	defer srv.Close()

	c := apiclient.New(srv.URL, "test-token")
	out, err := createNote(context.Background(), c, createNoteIn{Body: "new note"})
	if err != nil {
		t.Fatalf("createNote: %v", err)
	}
	if out.Note.ID != 7 || out.Note.Body != "new note" {
		t.Errorf("unexpected note: %+v", out.Note)
	}
}

func TestCreateNoteAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"body required"}`)
	}))
	defer srv.Close()

	c := apiclient.New(srv.URL, "test-token")
	_, err := createNote(context.Background(), c, createNoteIn{Body: ""})
	if err == nil {
		t.Fatal("expected an error on non-2xx response")
	}
}
