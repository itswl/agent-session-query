package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/render"
)

// serverCheck is what doctor learned of a server on this machine: which asq it runs, and
// whether that is this one. A server started before an upgrade goes on with the asq it was
// started from until it is restarted.
type serverCheck struct {
	Address  string `json:"address"`
	Answered bool   `json:"answered"`
	// Version and Executable are the asq it runs, and the file it was started from;
	// Replaced says an upgrade has been installed over that file since
	Version    string `json:"version,omitempty"`
	Executable string `json:"executable,omitempty"`
	Replaced   bool   `json:"replaced,omitempty"`
	// Restart says it runs another asq than this one, or one an upgrade replaced
	Restart bool `json:"restart,omitempty"`
	// Locked says it takes a token, and doctor holds none it takes (ASQ_TOKEN)
	Locked bool `json:"locked,omitempty"`
	// Other says what answered is not an asq server
	Other bool `json:"other,omitempty"`
}

// askServer asks the server at addr, on this machine, for its health report: without a
// token first, and with token only once the answer shows it is asq that wants one. Nothing
// leaves the machine.
func askServer(ctx context.Context, addr, token string) serverCheck {
	ck := serverCheck{Address: addr}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	get := func(bearer string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/health", nil)
		if err != nil {
			return nil, err
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		return client.Do(req)
	}
	resp, err := get("")
	if err != nil {
		return ck
	}
	ck.Answered = true
	asq := func(r *http.Response) bool {
		return r.StatusCode == http.StatusUnauthorized && strings.Contains(r.Header.Get("WWW-Authenticate"), `realm="asq"`)
	}
	if asq(resp) && token != "" {
		resp.Body.Close()
		if resp, err = get(token); err != nil {
			ck.Other = true
			return ck
		}
	}
	defer resp.Body.Close()
	if asq(resp) {
		ck.Locked = true
		return ck
	}
	var h struct {
		Version    string `json:"version"`
		Executable string `json:"executable"`
		Replaced   bool   `json:"replaced"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&h) != nil || h.Version == "" {
		ck.Other = true
		return ck
	}
	ck.Version, ck.Executable, ck.Replaced = h.Version, h.Executable, h.Replaced
	ck.Restart = h.Replaced || h.Version != app.Version
	return ck
}

// sayServer says what doctor learned of a server, in a line or two.
func sayServer(out io.Writer, ck serverCheck, token string, st render.Style) {
	const name = "server"
	on := "on " + ck.Address
	switch {
	case !ck.Answered:
		fmt.Fprintf(out, "· %-12s none answers %s\n", name, on)
	case ck.Other:
		fmt.Fprintf(out, "· %-12s something answers %s, and not as asq does\n", name, on)
	case ck.Locked && token != "":
		fmt.Fprintf(out, "· %-12s asq answers %s and takes a token, which ASQ_TOKEN is not\n", name, on)
	case ck.Locked:
		fmt.Fprintf(out, "· %-12s asq answers %s and takes a token: with it in ASQ_TOKEN, doctor says which asq it runs\n", name, on)
	case ck.Replaced:
		fmt.Fprintf(out, "%s %-12s asq %s %s was started before an upgrade replaced %s: restart it to run the new one\n", st.Warn("!"), name, ck.Version, on, st.Path(ck.Executable))
	case ck.Restart:
		self, _ := app.Program()
		if ck.Executable != "" && self != "" && !sameFile(ck.Executable, self) {
			fmt.Fprintf(out, "%s %-12s asq %s %s runs %s, and this is %s at %s: upgrade that one, or have the service start this one, and restart it\n",
				st.Warn("!"), name, ck.Version, on, st.Path(ck.Executable), app.Version, st.Path(self))
		} else {
			fmt.Fprintf(out, "%s %-12s asq %s %s, and this is %s: restart it to run this one\n", st.Warn("!"), name, ck.Version, on, app.Version)
		}
	default:
		fmt.Fprintf(out, "✓ %-12s asq %s %s   %s\n", name, ck.Version, on, st.Path(ck.Executable))
	}
}

// sameFile reports whether two paths name one file, however each was spelled: macOS names
// /tmp also /private/tmp, and a process started by a link is named by the link or by
// where it leads. Paths that cannot be looked at are compared as written.
func sameFile(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return os.SameFile(ia, ib)
}
