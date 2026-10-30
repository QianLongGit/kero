package daemon

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// A pane can run a tmux client: the program in the pane is tmux, and the
// work happens on the host's tmux server, in sessions of panes kero's
// detection never sees. What tmux says of itself — its client terminals
// and its panes — is that topology, read here as one snapshot taken again
// every so often, which the panes' questions (which agent works in the
// session this pane shows? which pane did that hook come from?) answer
// from. Nothing here may fail a caller: a host without tmux, or one whose
// tmux is away, answers an empty snapshot like any other.

// tmuxClient is a terminal tmux is attached to, and the session it
// watches.
type tmuxClient struct {
	TTY     string
	Session string
}

// tmuxPane is one pane of a tmux session: what runs in it — Command now,
// StartCommand as its shell started it — and whether it is one a user
// looks at, the active pane of its session's active window.
type tmuxPane struct {
	TTY          string
	Session      string
	SessionID    string
	Command      string
	StartCommand string
	WindowActive bool
	PaneActive   bool
}

// tmuxSnapshot is the host's tmux at a moment: every client terminal, and
// every pane of every session. Ttys join it all — a kero pane's pty slave
// path is a client's tty, and a hook's tty a pane's.
type tmuxSnapshot struct {
	Clients []tmuxClient
	Panes   []tmuxPane
}

// clientSession returns the name of the session the client on tty watches,
// "" when no client is on it.
func (s *tmuxSnapshot) clientSession(tty string) string {
	for _, c := range s.Clients {
		if c.TTY == tty {
			return c.Session
		}
	}
	return ""
}

// sessionPanes returns the panes of the named session, the active pane of
// the active window first: the order a user looking at the session sees.
func (s *tmuxSnapshot) sessionPanes(session string) []tmuxPane {
	var out []tmuxPane
	for _, p := range s.Panes {
		if p.Session == session {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(a, b tmuxPane) int { return tmuxRank(a) - tmuxRank(b) })
	return out
}

// tmuxRank orders a session's panes: its active window's active pane
// first, then the active window's other panes, then the windows no one
// looks at.
func tmuxRank(p tmuxPane) int {
	switch {
	case p.WindowActive && p.PaneActive:
		return 0
	case p.WindowActive:
		return 1
	default:
		return 2
	}
}

// paneSession returns the session of the pane on tty, by name and id, ""
// for none.
func (s *tmuxSnapshot) paneSession(tty string) (name, id string) {
	for _, p := range s.Panes {
		if p.TTY == tty {
			return p.Session, p.SessionID
		}
	}
	return "", ""
}

// sessionName returns the name of the session with tmux's id for it, ""
// for none: the id is stable, the name a user may rename.
func (s *tmuxSnapshot) sessionName(id string) string {
	for _, p := range s.Panes {
		if p.SessionID == id {
			return p.Session
		}
	}
	return ""
}

// parseClients reads what tmux answers for
// list-clients -F '#{client_tty}\t#{client_session}': a client a line.
// A line that is empty or short of its fields is dropped — a surprise
// from tmux, not a failure of the reading.
func parseClients(out string) []tmuxClient {
	var clients []tmuxClient
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 2)
		if len(f) < 2 || f[0] == "" {
			continue
		}
		clients = append(clients, tmuxClient{TTY: f[0], Session: f[1]})
	}
	return clients
}

// parsePanes reads what tmux answers for
// list-panes -a -F '#{pane_tty}\t#{session_name}\t#{session_id}\t#{window_active}\t#{pane_active}\t#{pane_current_command}\t#{pane_start_command}':
// a pane a line, of every session (-a), the two active flags as tmux
// writes them, 1 and 0. A line short of its seven fields is dropped; the
// start command is the last field, and may itself hold tabs, so the line
// is split into no more than the seven.
func parsePanes(out string) []tmuxPane {
	var panes []tmuxPane
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 7)
		if len(f) < 7 || f[0] == "" {
			continue
		}
		panes = append(panes, tmuxPane{
			TTY: f[0], Session: f[1], SessionID: f[2],
			WindowActive: f[3] == "1", PaneActive: f[4] == "1",
			Command: f[5], StartCommand: f[6],
		})
	}
	return panes
}

// tmuxCache holds one snapshot of the host's tmux, taken again only every
// ttl: every pane's tick asks, and the host's tmux is not asked about the
// same moment twice. A query that fails — tmux away, or slow past its
// deadline — keeps the last snapshot standing and starts the window anew,
// so the answers age out gently rather than flap. The zero cache, with no
// runner, answers an empty snapshot.
type tmuxCache struct {
	run func(ctx context.Context, args ...string) (string, error)
	ttl time.Duration

	mu   sync.Mutex
	at   time.Time
	snap *tmuxSnapshot
}

// get returns the snapshot for now, refreshing it once its window has
// passed. The lock spans the queries, so a refresh is one and those that
// arrive while it runs read its result.
func (c *tmuxCache) get(now time.Time) *tmuxSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run == nil {
		return &tmuxSnapshot{}
	}
	if c.snap != nil && now.Sub(c.at) < c.ttl {
		return c.snap
	}
	ctx := context.Background()
	clients, cerr := c.run(ctx, "list-clients", "-F", "#{client_tty}\t#{client_session}")
	panes, perr := c.run(ctx, "list-panes", "-a", "-F",
		"#{pane_tty}\t#{session_name}\t#{session_id}\t#{window_active}\t#{pane_active}\t#{pane_current_command}\t#{pane_start_command}")
	if cerr == nil && perr == nil {
		c.snap = &tmuxSnapshot{Clients: parseClients(clients), Panes: parsePanes(panes)}
	} else if c.snap == nil {
		c.snap = &tmuxSnapshot{}
	}
	c.at = now
	return c.snap
}

// tmuxRun is the cache's runner: the tmux binary's path, looked for once —
// a host without it fails every query, which the cache holds as an empty
// snapshot — asked under a deadline of its own, its combined output (a
// complaint of tmux's included) trimmed.
func tmuxRun() func(ctx context.Context, args ...string) (string, error) {
	look := sync.OnceValues(func() (string, error) { return exec.LookPath("tmux") })
	return func(ctx context.Context, args ...string) (string, error) {
		bin, err := look()
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
}
