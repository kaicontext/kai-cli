package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/clientid"
)

// kai names itself "cli" with its version, so the engine sends
// X-Kai-Client: cli/<version>, and cost reports still carry the version.
func TestSetClient(t *testing.T) {
	prev := agent.ClientVersion()
	t.Cleanup(func() { agent.SetClientVersion(prev); clientid.Set("", "") })
	setClient()
	if got, want := clientid.Value(), "cli/"+Version; got != want {
		t.Errorf("X-Kai-Client = %q, want %q", got, want)
	}
	if got := agent.ClientVersion(); got != Version {
		t.Errorf("ClientVersion = %q, want %q", got, Version)
	}
}

// kai's own calls to the server carry the header, and drop it on a redirect
// to another host.
func TestServerCallSendsClientHeader(t *testing.T) {
	prev := agent.ClientVersion()
	t.Cleanup(func() { agent.SetClientVersion(prev); clientid.Set("", "") })
	setClient()

	var other string
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other = r.Header.Get(clientid.Header)
		w.Write([]byte(`{}`))
	}))
	defer otherSrv.Close()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(clientid.Header)
		if r.URL.Path == "/away" {
			http.Redirect(w, r, otherSrv.URL+"/x", http.StatusFound)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := serverCallRaw(srv.URL, "tok", "GET", "/here", nil); err != nil {
		t.Fatal(err)
	}
	if want := "cli/" + Version; got != want {
		t.Errorf("server saw X-Kai-Client %q, want %q", got, want)
	}
	if _, err := serverCallRaw(srv.URL, "tok", "GET", "/away", nil); err != nil {
		t.Fatal(err)
	}
	if other != "" {
		t.Errorf("other host saw X-Kai-Client %q, want none", other)
	}
}
