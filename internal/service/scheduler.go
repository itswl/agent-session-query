package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// scheduler keeps a task in Task Scheduler's root folder, started at this user's logon and
// run as them, in their session: a task that runs whether or not they are logged on takes
// their password, or an administrator, to create.
//
// Task Scheduler's defaults are for work that ends, and the definition undoes each: no
// time limit (72 hours by default), started and kept running on battery, a start that
// fails tried again every minute, and priority 5, which is normal (7, below it, by
// default).
type scheduler struct{ m *Machine }

func (t *scheduler) name() string  { return "Task Scheduler" }
func (t *scheduler) where() string { return `\` + Task }
func (t *scheduler) log() string   { return "" }

const taskNS = "http://schemas.microsoft.com/windows/2004/02/mit/task"

// render writes the task as Task Scheduler's XML. It says it is UTF-16, which is what
// write gives schtasks; the text here is UTF-8, as Go holds it. The program is quoted
// when its path has a space, as Task Scheduler's own window writes it.
func (t *scheduler) render(s Setup) []byte {
	user := esc(t.m.User)
	var args []string
	for _, a := range s.Args {
		args = append(args, winQuote(a))
	}
	return []byte(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="` + taskNS + `">
  <RegistrationInfo>
    <Description>` + Mark + `: asq service uninstall removes it, and asq service install writes it anew</Description>
    <URI>\` + Task + `</URI>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + user + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + user + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>5</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(winQuote(s.Binary)) + `</Command>
      <Arguments>` + esc(strings.Join(args, " ")) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`)
}

// parse reads back what a task runs: its first Exec action. The environment is the
// user's, and not in the task.
func (t *scheduler) parse(def []byte) Setup {
	var task struct {
		Exec []struct {
			Command   string `xml:"Command"`
			Arguments string `xml:"Arguments"`
		} `xml:"Actions>Exec"`
	}
	d := xml.NewDecoder(bytes.NewReader(utf8Of(def)))
	// What it declares is how it was written, not how it reads now: utf8Of made it UTF-8
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	if err := d.Decode(&task); err != nil || len(task.Exec) == 0 {
		return Setup{}
	}
	return Setup{Binary: strings.Trim(task.Exec[0].Command, `"`), Args: winSplit(task.Exec[0].Arguments)}
}

// current is the task as Task Scheduler holds it now. PowerShell gives it whatever the
// language of the system; schtasks /query would say it in that language.
func (t *scheduler) current() ([]byte, bool) {
	out, err := t.ps(`Export-ScheduledTask -TaskPath '\' -TaskName '` + Task + `'`)
	if err != nil || !bytes.Contains(out, []byte("<Task")) {
		return nil, false
	}
	return utf8Of(out), true
}

// write registers the task, replacing one of the name: schtasks reads it from a file, in
// UTF-16 as the file says.
func (t *scheduler) write(def []byte) error {
	f, err := os.CreateTemp(t.m.Temp, "asq-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(utf16Of(def))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if out, err := t.m.Run("schtasks", "/create", "/tn", Task, "/xml", f.Name(), "/f"); err != nil {
		return fmt.Errorf("schtasks /create: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// load starts the task, ending a running one first: it would go on with what it was
// started with, and while it runs a start is ignored.
func (t *scheduler) load() ([]string, error) {
	var ran []string
	if _, state, _ := t.state(); state == "running" {
		t.m.Run("schtasks", "/end", "/tn", Task)
		ran = append(ran, said("schtasks", "/end", "/tn", Task))
		for range 50 {
			if _, state, _ := t.state(); state != "running" {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	ran = append(ran, said("schtasks", "/run", "/tn", Task))
	if out, err := t.m.Run("schtasks", "/run", "/tn", Task); err != nil {
		return ran, fmt.Errorf("schtasks /run: %v %s", err, strings.TrimSpace(string(out)))
	}
	return ran, nil
}

// state reads the task's state and its last result, as PowerShell names them whatever the
// language of the system.
func (t *scheduler) state() (bool, string, int) {
	out, err := t.ps(`$t = Get-ScheduledTask -TaskPath '\' -TaskName '` + Task + `'; "$($t.State) $(($t | Get-ScheduledTaskInfo).LastTaskResult)"`)
	if err != nil {
		return false, "", 0
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return false, "", 0
	}
	state := strings.ToLower(fields[len(fields)-min(2, len(fields))])
	if len(fields) >= 2 && state != "running" {
		// 0 is a clean end; 0x41301 is running and 0x41303 never run, which say nothing more
		if code, ok := lastResult(fields[len(fields)-1]); ok && code != 0 && code != 0x41301 && code != 0x41303 {
			state += fmt.Sprintf(", last result 0x%X", code)
		}
	}
	return true, state, 0
}

// lastResult reads a task's last result: a 32-bit code, printed in decimal — unsigned as
// PowerShell gives it, or signed, as an HRESULT handed over as an int reads. Anything else
// is no result at all.
func lastResult(s string) (uint32, bool) {
	if u, err := strconv.ParseUint(s, 10, 32); err == nil {
		return uint32(u), true
	}
	if n, err := strconv.ParseInt(s, 10, 32); err == nil {
		return uint32(n), true
	}
	return 0, false
}

func (t *scheduler) unload() []string {
	t.m.Run("schtasks", "/end", "/tn", Task)
	return []string{said("schtasks", "/end", "/tn", Task)}
}

func (t *scheduler) remove() ([]string, error) {
	cmd := said("schtasks", "/delete", "/tn", Task, "/f")
	if out, err := t.m.Run("schtasks", "/delete", "/tn", Task, "/f"); err != nil {
		return []string{cmd}, fmt.Errorf("schtasks /delete: %v %s", err, strings.TrimSpace(string(out)))
	}
	return []string{cmd}, nil
}

func (t *scheduler) loadSteps() []string {
	return []string{said("schtasks", "/create", "/tn", Task, "/xml", "asq.xml", "/f"), said("schtasks", "/run", "/tn", Task)}
}

func (t *scheduler) unloadSteps() []string {
	return []string{said("schtasks", "/end", "/tn", Task), said("schtasks", "/delete", "/tn", Task, "/f")}
}

// ps runs a line of Windows PowerShell, which every Windows since 7 has, with what it
// prints in UTF-8 and any error ending it unsuccessfully.
func (t *scheduler) ps(script string) ([]byte, error) {
	return t.m.Run("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"$ErrorActionPreference = 'Stop'; [Console]::OutputEncoding = [Text.Encoding]::UTF8; "+script)
}

// utf16Of is text as UTF-16, little-endian, after its byte order mark.
func utf16Of(text []byte) []byte {
	units := utf16.Encode([]rune(string(text)))
	out := make([]byte, 0, 2+2*len(units))
	out = append(out, 0xFF, 0xFE)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// utf8Of is text as UTF-8: as it is, or from UTF-16 when a byte order mark says it is.
func utf8Of(b []byte) []byte {
	var big bool
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		big = true
	default:
		return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	}
	b = b[2:]
	units := make([]uint16, len(b)/2)
	for i := range units {
		if big {
			units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			units[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
	}
	return []byte(string(utf16.Decode(units)))
}

// winQuote is one argument as a Windows program's command line holds it: quoted when it
// has a space, a tab or a quote, with the backslashes before a quote doubled.
func winQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			b.WriteByte('"')
			slashes = 0
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			b.WriteByte(c)
			slashes = 0
		}
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}

// winSplit splits a command line into its arguments as a Windows program's C runtime does:
// 2n backslashes and a quote are n and a quote that opens or closes, 2n+1 and a quote are
// n and a quote, and backslashes before anything else are themselves.
func winSplit(s string) []string {
	var args []string
	var w strings.Builder
	quoted, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			n := 0
			for i < len(s) && s[i] == '\\' {
				n++
				i++
			}
			if i < len(s) && s[i] == '"' {
				w.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					w.WriteByte('"')
				} else {
					quoted = !quoted
				}
			} else {
				w.WriteString(strings.Repeat(`\`, n))
				i--
			}
			have = true
		case c == '"':
			quoted, have = !quoted, true
		case !quoted && (c == ' ' || c == '\t'):
			if have {
				args = append(args, w.String())
				w.Reset()
				have = false
			}
		default:
			w.WriteByte(c)
			have = true
		}
	}
	if have {
		args = append(args, w.String())
	}
	return args
}
