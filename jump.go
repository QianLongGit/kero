package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo"

	"kero/internal/proto"
)

// jumpQuery is what a kero://focus URL jumps to: the coding agent's
// session, the terminal device, or the directory, any of which may be
// empty and then plays no part.
type jumpQuery struct{ session, tty, cwd string }

// parseJumpURL reads a jump out of a kero://focus URL, as Vibe Island
// writes one. ok is false for anything else: another scheme or host, or
// not a URL at all. Parameters are URL-decoded; absent and empty ones
// read the same.
func parseJumpURL(u string) (jumpQuery, bool) {
	parsed, err := url.Parse(u)
	if err != nil || !strings.EqualFold(parsed.Scheme, "kero") || !strings.EqualFold(parsed.Host, "focus") {
		return jumpQuery{}, false
	}
	p := parsed.Query()
	return jumpQuery{session: p.Get("session"), tty: p.Get("tty"), cwd: p.Get("cwd")}, true
}

// resolveJump finds the pane a jump query names, in Vibe Island's
// documented order: the agent session, on any host's pane; the terminal
// device, on a local one; the working directory, with local panes first
// and the most recently active of them, state order breaking what is
// still a tie. An empty parameter skips its level; nothing matching
// gives no session and no pane.
func resolveJump(st *proto.State, local func(paneID string) bool, q jumpQuery) (session, pane string) {
	for i := range st.Sessions {
		s := &st.Sessions[i]
		for j := range s.Panes {
			if p := &s.Panes[j]; q.session != "" && p.AgentSession != nil && p.AgentSession.ID == q.session {
				return s.ID, p.ID
			}
		}
	}
	for i := range st.Sessions {
		s := &st.Sessions[i]
		for j := range s.Panes {
			if p := &s.Panes[j]; q.tty != "" && local(p.ID) && p.TTY == q.tty {
				return s.ID, p.ID
			}
		}
	}
	var best *proto.Pane
	var bestSession string
	for i := range st.Sessions {
		s := &st.Sessions[i]
		for j := range s.Panes {
			p := &s.Panes[j]
			if q.cwd == "" || p.WorkDir() != q.cwd {
				continue
			}
			// Strict comparison: what ties on locality and activity keeps
			// the pane the state lists first.
			if best == nil || (local(p.ID) && !local(best.ID)) ||
				(local(p.ID) == local(best.ID) && p.Activity > best.Activity) {
				best, bestSession = p, s.ID
			}
		}
	}
	if best != nil {
		return bestSession, best.ID
	}
	return "", ""
}

// openJump goes to what a kero://focus URL names, as Vibe Island asks of
// the app: the pane its state holds, by agent session, device or
// directory, its session selected and its window in front.
func (a *App) openJump(url string) {
	q, ok := parseJumpURL(url)
	if !ok {
		return
	}
	// mygo hands launch URLs over after the windows it opens when ready,
	// and on the app's thread; a window not there is a test's, which holds
	// the jump as a state not there does.
	if a.win == nil {
		a.pending = &q
		return
	}
	a.update(func() {
		if !a.loaded {
			a.pending = &q
			return
		}
		a.jumpToPane(q)
	})
}

// takePendingJump takes the jump a URL left waiting for the state, as a
// host's state comes: one the state resolves goes there at once. This
// machine's settles every jump — a pane it has not means none exists,
// and the window alone comes up — while another host's may be yet to
// come, and the wait goes on.
func (a *App) takePendingJump(h *host) {
	if a.pending == nil {
		return
	}
	s, _ := resolveJump(&a.st, a.localPane, *a.pending)
	if s == "" && h.remote() {
		return
	}
	q := *a.pending
	a.pending = nil
	a.jumpToPane(q)
}

// jumpToPane goes to the pane a jump names: its session selected, the
// user in the pane, the window in front. One nothing matches brings the
// window up alone.
func (a *App) jumpToPane(q jumpQuery) {
	s, pane := resolveJump(&a.st, a.localPane, q)
	if s != "" {
		a.selectSession(s)
		// Whether the pane is the one the app already has: what it and
		// kerod hold of the focus stays in step, and the jump lands the
		// same.
		a.setFocus(a.session(s), pane)
	}
	a.showWindow()
}

// localPane tells whether a pane of the state runs on this machine, as a
// jump's device and directory levels ask: a pane no host has is not one
// of this machine's.
func (a *App) localPane(id string) bool {
	h := a.owner[id]
	return h != nil && h.name == ""
}

// vibeIslandSetup is what Kero's Vibe Island rule is built of: this
// build's app name, the file its rule goes in — named after the daemon's
// directory identity, so dev and installed Kero write beside each other —
// and the bundle identifier Vibe Island opens.
func vibeIslandSetup() (displayName, fileName, bundleID string) {
	bundleID = "sh.kero"
	if mygo.IsDev() {
		bundleID += ".dev"
	}
	return mygo.App.Name(), proto.Name() + ".json", bundleID
}

// vibeIslandJumpURI is the rule's jumpRule member: how Vibe Island jumps
// here, and with which of the state's handles.
type vibeIslandJumpURI struct {
	Method   string `json:"method"`
	Template string `json:"template"`
}

// vibeIslandRuleFile is the rule file, its fields in the order Vibe
// Island's docs present them, so the file reads like the page it
// documents.
type vibeIslandRuleFile struct {
	DisplayName      string            `json:"displayName"`
	BundleIdentifier string            `json:"bundleIdentifier"`
	JumpRule         vibeIslandJumpURI `json:"jumpRule"`
}

// vibeIslandRule writes the rule Vibe Island reads to jump here: the
// focus URL of the jump above, with the session, device and directory
// its state holds. A URL template wants its &s as written, not the HTML
// escapes encoding/json defaults to.
func vibeIslandRule(displayName, bundleID string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(vibeIslandRuleFile{
		DisplayName:      displayName,
		BundleIdentifier: bundleID,
		JumpRule: vibeIslandJumpURI{
			Method:   "urlScheme",
			Template: "kero://focus?session={session_id}&tty={tty}&cwd={cwd}",
		},
	})
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")) // Encode's trailing newline
}

// vibeIslandRulePath is where Vibe Island looks for an integration's
// rule, in the real home it reads — a fixed path, not one of Kero's
// configurable homes.
func vibeIslandRulePath(home, fileName string) string {
	return filepath.Join(home, ".vibe-island", "integrations", fileName)
}

// installVibeIslandRule writes the rule into Vibe Island's integrations
// directory, for its next start to pick up. The file is a fresh
// integration, not a config users edit: a plain write, no ceremony.
func installVibeIslandRule(home, displayName, fileName, bundleID string) error {
	path := vibeIslandRulePath(home, fileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, vibeIslandRule(displayName, bundleID), 0o644)
}

// removeVibeIslandRule takes the rule out of Vibe Island's integrations
// directory. A file already gone is one removed.
func removeVibeIslandRule(home, fileName string) error {
	if err := os.Remove(vibeIslandRulePath(home, fileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// vibeIslandRuleState tells what the rule on disk is against the one
// this build writes: none there, the same bytes, or an older Kero's.
func vibeIslandRuleState(home, fileName string, want []byte) string {
	got, err := os.ReadFile(vibeIslandRulePath(home, fileName))
	if err != nil {
		return "missing"
	}
	if bytes.Equal(got, want) {
		return "current"
	}
	return "outdated"
}
