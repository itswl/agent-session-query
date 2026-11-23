package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// launchd keeps a LaunchAgent: a plist in the user's own ~/Library/LaunchAgents, loaded
// into the user's GUI domain, so that it runs as the user, while they are logged in. A
// LaunchDaemon would run as root, whose home is not the user's, and read nobody's sessions.
type launchd struct{ m *Machine }

func (l *launchd) name() string   { return "launchd" }
func (l *launchd) where() string  { return l.library("LaunchAgents", Label+".plist") }
func (l *launchd) log() string    { return l.library("Logs", "asq.log") }
func (l *launchd) domain() string { return "gui/" + strconv.Itoa(l.m.UID) }
func (l *launchd) target() string { return l.domain() + "/" + Label }

func (l *launchd) library(dir, file string) string {
	return filepath.Join(l.m.Home, "Library", dir, file)
}

// render writes what docs/deploy.md has a person write by hand. A plist expands nothing —
// not ~, not a variable — so every path in it is absolute.
func (l *launchd) render(s Setup) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- ` + Mark + `: asq service uninstall removes it, and asq service install writes it anew -->
<plist version="1.0">
<dict>
  <key>Label</key><string>` + Label + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range append([]string{s.Binary}, s.Args...) {
		b.WriteString("    <string>" + esc(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	if len(s.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, v := range s.Env {
			b.WriteString("    <key>" + esc(v.Name) + "</key><string>" + esc(v.Value) + "</string>\n")
		}
		b.WriteString("  </dict>\n")
	}
	b.WriteString(`  <key>RunAtLoad</key><true/>
  <!-- Started again after an abnormal exit only -->
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <!-- Standard, not Background: it answers what a person or an agent asks, and Background
       throttles it as work nobody is waiting for -->
  <key>ProcessType</key><string>Standard</string>
  <key>StandardErrorPath</key><string>` + esc(l.log()) + `</string>
</dict>
</plist>
`)
	return b.Bytes()
}

// parse reads back what a plist runs: ProgramArguments, or Program alone, and the
// variables it sets.
func (l *launchd) parse(def []byte) Setup {
	var s Setup
	root, _ := readPlist(def).(plistDict)
	if args, ok := root.get("ProgramArguments").([]any); ok {
		for _, a := range args {
			if str, ok := a.(string); ok {
				s.Args = append(s.Args, str)
			}
		}
	}
	if program, ok := root.get("Program").(string); ok {
		if len(s.Args) == 0 {
			s.Args = []string{program}
		} else {
			s.Args[0] = program
		}
	}
	if len(s.Args) > 0 {
		s.Binary, s.Args = s.Args[0], s.Args[1:]
	}
	if env, ok := root.get("EnvironmentVariables").(plistDict); ok {
		for _, kv := range env {
			if str, ok := kv.value.(string); ok {
				s.Env = append(s.Env, Var{kv.key, str})
			}
		}
	}
	return s
}

func (l *launchd) current() ([]byte, bool) { return readFile(l.where()) }

// write puts the plist in place, and the directory its log goes in: launchd makes none.
func (l *launchd) write(def []byte) error {
	if err := os.MkdirAll(filepath.Dir(l.log()), 0o755); err != nil {
		return err
	}
	return writeFile(l.where(), def)
}

// load has launchd take the plist and start it. One it holds already is let go of first:
// launchd reads a plist only when it is bootstrapped. bootout returns before the job is
// gone, and a bootstrap meanwhile fails, so the first is waited out.
func (l *launchd) load() ([]string, error) {
	var ran []string
	if loaded, _, _ := l.state(); loaded {
		l.m.Run("launchctl", "bootout", l.target())
		ran = append(ran, said("launchctl", "bootout", l.target()))
		for range 50 {
			if loaded, _, _ := l.state(); !loaded {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	ran = append(ran, said("launchctl", "bootstrap", l.domain(), l.where()))
	if out, err := l.m.Run("launchctl", "bootstrap", l.domain(), l.where()); err != nil {
		return ran, fmt.Errorf("launchctl bootstrap: %v %s", err, strings.TrimSpace(string(out)))
	}
	return ran, nil
}

// state reads launchctl print: its job's own lines are indented once, and what follows
// them — its endpoints, its environment — more.
func (l *launchd) state() (bool, string, int) {
	out, err := l.m.Run("launchctl", "print", l.target())
	if err != nil {
		return false, "", 0
	}
	state, exit, pid := "", "", 0
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "state":
			state = value
		case "pid":
			pid, _ = strconv.Atoi(value)
		case "last exit code":
			exit = value
		}
	}
	if state != "running" && exit != "" && !strings.HasPrefix(exit, "(") {
		state += ", last exit code " + exit
	}
	return true, state, pid
}

func (l *launchd) unload() []string {
	l.m.Run("launchctl", "bootout", l.target())
	return []string{said("launchctl", "bootout", l.target())}
}

func (l *launchd) remove() ([]string, error) { return nil, removeFile(l.where()) }

func (l *launchd) loadSteps() []string {
	return []string{said("launchctl", "bootstrap", l.domain(), l.where())}
}

func (l *launchd) unloadSteps() []string {
	return []string{said("launchctl", "bootout", l.target()), said("rm", l.where())}
}

// esc is text as XML holds it.
func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// A plist's dict, in its order.
type plistDict []plistPair

type plistPair struct {
	key   string
	value any
}

func (d plistDict) get(key string) any {
	for _, kv := range d {
		if kv.key == key {
			return kv.value
		}
	}
	return nil
}

// readPlist reads an XML property list: a dict as a plistDict, an array as a []any,
// true and false as bools, anything else as its text. What it cannot read is nil.
func readPlist(b []byte) any {
	d := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := d.Token()
		if err != nil {
			return nil
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local != "plist" {
			v, _ := plistValue(d, start)
			return v
		}
	}
}

func plistValue(d *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "dict":
		var dict plistDict
		key := ""
		for {
			tok, err := d.Token()
			if err != nil {
				return dict, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					var k string
					if err := d.DecodeElement(&k, &t); err != nil {
						return dict, err
					}
					key = k
					continue
				}
				v, err := plistValue(d, t)
				if err != nil {
					return dict, err
				}
				dict = append(dict, plistPair{key, v})
			case xml.EndElement:
				return dict, nil
			}
		}
	case "array":
		var list []any
		for {
			tok, err := d.Token()
			if err != nil {
				return list, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := plistValue(d, t)
				if err != nil {
					return list, err
				}
				list = append(list, v)
			case xml.EndElement:
				return list, nil
			}
		}
	case "true", "false":
		return start.Name.Local == "true", d.Skip()
	}
	var text string
	err := d.DecodeElement(&text, &start)
	return text, err
}
