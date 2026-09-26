package main

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewDependenciesIncludeUnchangedPins(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0755)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example\nrequire github.com/kaicontext/kai-engine v0.6.74\n")
	write("cmd/ship.go", "package main\nimport gitio \"github.com/kaicontext/kai-engine/gitio\"\nfunc ship(){gitio.DiscardChanges(\".\")}\n")
	deps := rcReviewDeps(root, "diff touching only the call site", []string{"cmd/ship.go"})
	if len(deps) != 1 || deps[0].To != "v0.6.74" || strings.Join(deps[0].Pkgs, ",") != "gitio" {
		t.Fatalf("unchanged pin lost: %+v", deps)
	}
	write("cmd/go.mod", "module nested\n")
	if got := rcReviewDeps(root, "", []string{"cmd/ship.go"}); len(got) != 0 {
		t.Fatalf("nested module assigned parent pins: %+v", got)
	}
	os.Remove(filepath.Join(root, "cmd/go.mod"))
	write("go.mod", "module example\nrequire github.com/kaicontext/kai-engine v0.6.74\nreplace github.com/kaicontext/kai-engine => ../engine\n")
	if got := rcReviewDeps(root, "", []string{"cmd/ship.go"}); len(got) != 0 {
		t.Fatalf("replacement assigned upstream source: %+v", got)
	}
}

type rcDepTransport func(*http.Request) (*http.Response, error)

func (f rcDepTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDependencyReleaseTagFetchUsesResolvedCommit(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	old := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = old })
	sha := strings.Repeat("a", 40)
	var paths []string
	http.DefaultClient = &http.Client{Transport: rcDepTransport(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		body := ""
		if strings.HasSuffix(r.URL.Path, "/commits/v0.6.74") {
			body = `{"sha":"` + sha + `"}`
		} else {
			if r.URL.Query().Get("ref") != sha {
				t.Fatalf("source fetched at floating/wrong ref: %s", r.URL)
			}
			body = `[{"name":"gitio.go","path":"gitio/gitio.go","type":"file","size":20,"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("package gitio\n")) + `"}]`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	got, unresolved := rcFetchDepSources(context.Background(), []rcDepChange{{Module: "github.com/kaicontext/kai-engine", To: "v0.6.74", Pkgs: []string{"gitio"}}})
	if len(got) != 1 || len(unresolved) != 0 || len(paths) != 2 {
		t.Fatalf("tag fetch: %+v unresolved=%+v paths=%v", got, unresolved, paths)
	}
	http.DefaultClient = &http.Client{Transport: rcDepTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	got, unresolved = rcFetchDepSources(context.Background(), []rcDepChange{{Module: "github.com/kaicontext/kai-engine", To: "v0.6.74", Pkgs: []string{"gitio"}}})
	if len(got) != 0 || len(unresolved) != 1 {
		t.Fatal("missing tag must stay unresolved, never fall back to main")
	}
}
