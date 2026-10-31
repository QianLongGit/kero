package main

import (
	"time"

	"kero/internal/proto"

	"kero/locales"
)

// hostHooksLook is a host's agent hooks as Settings › Hosts last asked:
// the state its kerod told of, and when, "" while none has come.
type hostHooksLook struct {
	state   string
	at      time.Time
	looking bool
}

// hooksDetail words the state of a host's agent hooks for its row: the
// skills' words for the states they share, the row's own while the host
// hasn't said.
func hooksDetail(state string) string {
	switch state {
	case "current":
		return locales.SetSkillInstalled()
	case "outdated":
		return locales.SetSkillOutdated()
	case "missing":
		return locales.SetSkillMissing()
	}
	return locales.SetHostHooksDetail()
}

// hooksOffers says what the hooks row offers in a state: a remove while
// hooks are on the host, an install — an update, when an older Kero wrote
// the ones there — while they aren't current, and nothing while unknown.
func hooksOffers(state string) (remove, install bool) {
	return state == "current" || state == "outdated", state == "missing" || state == "outdated"
}

// hooksLook is the host's agent hooks as the Hosts page keeps them, made
// on first use.
func (a *App) hooksLook(h *host) *hostHooksLook {
	if a.ss.hooks == nil {
		a.ss.hooks = map[string]*hostHooksLook{}
	}
	l := a.ss.hooks[h.name]
	if l == nil {
		l = &hostHooksLook{}
		a.ss.hooks[h.name] = l
	}
	return l
}

// refreshHostHooks asks a host's kerod how its agent hooks stand, at most
// once a while, off the main thread, as the Hosts page builds its rows.
func (a *App) refreshHostHooks(h *host) {
	l := a.hooksLook(h)
	cl := h.cl
	if cl == nil || l.looking || time.Since(l.at) < 30*time.Second {
		return
	}
	l.looking = true
	go func() {
		var r proto.HooksResult
		err := cl.Call("hooks.state", proto.HooksParams{Agent: "claude"}, &r)
		a.update(func() {
			l.at, l.looking = time.Now(), false
			if err == nil {
				l.state = r.State
			}
			if a.setWin != nil {
				a.setWin.Invalidate()
			}
		})
	}()
}

// hostRemoveWait bounds how long removing a host waits for its kerod to
// take kero's hooks back: a kerod slow to answer must not hold the host.
var hostRemoveWait = 2 * time.Second

// removeHostAfterHooks takes a host out of kero's settings after asking
// its kerod, best-effort, to take kero's agent hooks back: a host gone
// from kero would otherwise keep them alive by its startup ensure
// forever. A host the app can't reach, one that errs or never answers,
// goes all the same. It runs off the app thread, as the confirmation
// that asks for it runs on it, and lands the removal back on it.
func (a *App) removeHostAfterHooks(h *host, name string) {
	if cl := h.cl; cl != nil {
		// Call waits on the daemon with no deadline of its own: bound the
		// wait, leaving the call's goroutine to end as the connection
		// does. The client stays — the app shares it.
		ch := make(chan error, 1)
		go func() {
			ch <- cl.Call("hooks.remove", proto.HooksParams{Agent: "claude"}, nil)
		}()
		select {
		case <-ch:
		case <-time.After(hostRemoveWait):
		}
	}
	a.update(func() { a.removeHost(name) })
}

// hostHooksAction installs or removes a host's agent hooks, as its row's
// buttons or its menu ask, then asks their state again. A host the app
// can't reach says so, without a call.
func (a *App) hostHooksAction(h *host, install bool) {
	method := "hooks.remove"
	if install {
		method = "hooks.setup"
	}
	a.callOn(h, method, proto.HooksParams{Agent: "claude"}, func(err error) {
		if err != nil {
			a.toast(err.Error())
			return
		}
		// What the host did changed the row: ask again, now.
		l := a.hooksLook(h)
		l.state, l.at = "", time.Time{}
		a.refreshHostHooks(h)
	})
}
