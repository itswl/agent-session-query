package app

import (
	"embed"
	"regexp"
	"strings"
	"testing"
)

//go:embed ui/app.js
var uiJS embed.FS

// app.js is plain script tags, so a function can be deleted while every call site
// survives: go test stays green and the page boots into an empty list. That is exactly
// what removing the storage panel once did — the deletion swept up refresh(), the boot
// path died on a ReferenceError, and the page showed nothing. Node cannot be assumed on
// the machine running the tests, so this guards names, not execution: every function the
// boot-and-refresh path calls through must still be defined in the file.
var jsFnDefined = regexp.MustCompile(`(?m)^\s*(?:async\s+)?function\s+([A-Za-z0-9_]+)\s*\(`)

func TestAppJSBootPathFunctionsDefined(t *testing.T) {
	src, err := uiJS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	for _, m := range jsFnDefined.FindAllStringSubmatch(string(src), -1) {
		defined[m[1]] = true
	}

	// The chain boot() → start() → refresh() and the helpers around it. When one of
	// these goes missing the page renders its chrome and nothing else — add a name here
	// the day the boot path grows another step.
	bootPath := []string{
		"applyTheme", "showGate", "showApp", "start",
		"applyViewPrefs", "showPane", "applyPaneFolds", "syncFoldAllButton",
		"selectSession", "startAutoRefresh", "startLiveFollow",
		"refresh", "handleError", "serverChanged", "scheduleCountWarmup",
		"renderSources", "renderList", "indexAgents", "syncArchivedButton",
		"syncDetail", "setStatus", "idleStatus", "api",
	}
	missing := []string{}
	for _, name := range bootPath {
		if !defined[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("app.js no longer defines %s — the page boots into an empty list without them (refresh() was deleted this way once)",
			strings.Join(missing, ", "))
	}
}
