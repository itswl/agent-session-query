package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/server"
)

func portOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

// doctor asks a server on this machine which asq it runs, and says when that is not the one
// installed now: one started before an upgrade goes on with the old one until restarted.
func TestDoctorAsksTheServerWhichAsqItRuns(t *testing.T) {
	h, indexPath := setup(t)
	self, _ := app.Program()
	const tok = "a-token-long-enough-to-be-one"
	answering := func(body string, token string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/health" {
				http.NotFound(w, r)
				return
			}
			if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
				w.Header().Set("WWW-Authenticate", `Bearer realm="asq"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	report := func(version, exe string, replaced bool) string {
		b, _ := json.Marshal(map[string]any{"version": version, "executable": exe, "replaced": replaced})
		return string(b)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closed, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()

	for _, tc := range []struct {
		about, port, token string
		want               []string
		restart            bool
	}{
		{"nothing listening", closed, "", []string{"· server       none answers on 127.0.0.1:" + closed}, false},
		{"this asq", portOf(t, answering(report(app.Version, self, false), "")), "", []string{"✓ server       asq " + app.Version + " on 127.0.0.1:"}, false},
		{"replaced since it started", portOf(t, answering(report(app.Version, self, true), "")), "",
			[]string{"! server       asq " + app.Version + " on 127.0.0.1:", "was started before an upgrade replaced", "restart it to run the new one"}, true},
		{"another install", portOf(t, answering(report("v1.4.0", "/opt/old/asq", false), "")), "",
			[]string{"! server       asq v1.4.0 on 127.0.0.1:", "runs /opt/old/asq, and this is " + app.Version, "upgrade that one"}, true},
		{"a token, not held", portOf(t, answering(report(app.Version, self, false), tok)), "", []string{"takes a token: with it in ASQ_TOKEN"}, false},
		{"a token, held", portOf(t, answering(report("v1.4.0", self, false), tok)), tok, []string{"! server       asq v1.4.0", "restart it to run this one"}, true},
		{"a token, another", portOf(t, answering(report(app.Version, self, false), tok)), "not-the-token-at-all-0123", []string{"which ASQ_TOKEN is not"}, false},
		{"not asq", portOf(t, answering("<html>hello</html>", "")), "", []string{"something answers on 127.0.0.1:", "not as asq does"}, false},
	} {
		t.Setenv("ASQ_TOKEN", tc.token)
		out, errs, code := asq(t, h, indexPath, "doctor", "--port", tc.port)
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: doctor says no %q:\n%s%s", tc.about, w, out, errs)
			}
		}
		if code != 0 {
			t.Errorf("%s: doctor exits %d: a server to restart is a warning", tc.about, code)
		}
		out, _, _ = asq(t, h, indexPath, "doctor", "--port", tc.port, "--json")
		var rep struct {
			Server struct {
				Address string `json:"address"`
				Restart bool   `json:"restart"`
			} `json:"server"`
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Server.Address != "127.0.0.1:"+tc.port || rep.Server.Restart != tc.restart {
			t.Errorf("%s: doctor --json: %v\n%s", tc.about, err, out)
		}
	}

	// Port 0 asks none
	out, errs, code := asq(t, h, indexPath, "doctor", "--port", "0")
	if code != 0 || strings.Contains(out, "server") {
		t.Errorf("doctor --port 0 asked a server: exit %d\n%s%s", code, out, errs)
	}
	out, _, _ = asq(t, h, indexPath, "doctor", "--port", "0", "--json")
	if strings.Contains(out, `"server"`) {
		t.Errorf("doctor --port 0 --json asked a server:\n%s", out)
	}
	// and so does ASQ_PORT=0, as setup sets it
	if out, errs, code := asq(t, h, indexPath, "doctor"); code != 0 || strings.Contains(out, "server") {
		t.Errorf("doctor with ASQ_PORT=0 asked a server: exit %d\n%s%s", code, out, errs)
	}

	// The real thing: what the server reports is what doctor reads
	t.Setenv("ASQ_TOKEN", "")
	a, _ := fixture.Open(t)
	s, err := server.New(a, server.Options{})
	if err != nil {
		t.Fatal(err)
	}
	real := httptest.NewServer(s)
	defer real.Close()
	if out, errs, _ := asq(t, h, indexPath, "doctor", "--port", portOf(t, real)); !strings.Contains(out, "✓ server       asq "+app.Version+" on 127.0.0.1:"+portOf(t, real)) {
		t.Errorf("doctor of a server of this asq:\n%s%s", out, errs)
	}

}
