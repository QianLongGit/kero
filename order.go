package main

import (
	"slices"
	"sort"

	"kero/internal/proto"
)

// orderSessions returns one sidebar group's sessions in display order:
// the auto order first (created ascending, or updated descending, as the
// sidebar's Order choice says), then every pinned session lifted out of
// it to stand directly after the session it is pinned to. A chain
// resolves anchor to tail, so no session can split it: one landing
// between the chain's places in the auto order stands after the tail.
// Pin "head" stands before the group's first item, head chains in auto
// order. A pin that cannot stand — pointing at a session that is gone,
// that lives in another group, at itself, or around a cycle, which no
// order honors — falls back to the auto place. The caller's slice is
// left as it was.
func orderSessions(group []proto.Session, order string) []proto.Session {
	base := slices.Clone(group)
	sort.SliceStable(base, func(i, j int) bool {
		if order == "updated" {
			return updatedKey(base[i]) > updatedKey(base[j])
		}
		return base[i].Created < base[j].Created
	})

	n := len(base)
	spot := make(map[string]int, n) // each session's place in the base order
	for i := range base {
		spot[base[i].ID] = i
	}
	// after[i]: where session i is pinned — its anchor's base spot, -1
	// for "head", -2 for auto or a pin that points nowhere in the group.
	after := make([]int, n)
	for i := range base {
		after[i] = -2
		switch p := base[i].Pin; p {
		case "":
		case "head":
			after[i] = -1
		default:
			if j, ok := spot[p]; ok {
				after[i] = j
			}
		}
	}

	// A cycle pins every member after another, which no order can honor:
	// void the pins around it so its sessions stand in auto order. A pin
	// from outside the cycle keeps standing — its anchor just stays where
	// the auto order put it.
	state := make([]int8, n) // 0 unvisited, 1 on this walk, 2 settled
	cycled := make([]bool, n)
	for i := range base {
		if state[i] != 0 {
			continue
		}
		var path []int
		j := i
		for state[j] == 0 {
			state[j] = 1
			path = append(path, j)
			if after[j] < 0 { // auto, head, or nowhere: the walk ends clean
				j = -1
				break
			}
			j = after[j]
		}
		// The whole walk stands, or the cycle on it does not — j, which
		// only this walk can hold at state 1, is where that cycle starts.
		first := len(path)
		if j >= 0 && state[j] == 1 {
			first = slices.Index(path, j)
		}
		for at, k := range path {
			cycled[k] = at >= first
			state[k] = 2
		}
	}

	// follow[anchor spot] holds the sessions pinned after the one there
	// (-1 is "head"), in base order; a chain is a follower followed by
	// its own.
	follow := make(map[int][]int, n)
	pinned := func(i int) bool { return after[i] >= -1 && !cycled[i] }
	for i := range base {
		if pinned(i) {
			follow[after[i]] = append(follow[after[i]], i)
		}
	}

	out := make([]proto.Session, 0, n)
	seen := make([]bool, n) // every session stands once; cycles are already void
	var emit func(i int)
	emit = func(i int) {
		if seen[i] {
			return
		}
		seen[i] = true
		out = append(out, base[i])
		for _, f := range follow[i] {
			emit(f)
		}
	}
	for _, f := range follow[-1] { // head chains stand before everything
		emit(f)
	}
	for i := range base { // the auto order, chains in their anchor's step
		if !pinned(i) {
			emit(i)
		}
	}
	return out
}

// updatedKey is when a session last did something, for the updated
// order: its program's last write, else its status's since, else its
// creation.
func updatedKey(s proto.Session) int64 {
	if s.Activity > 0 {
		return s.Activity
	}
	if s.Since > 0 {
		return s.Since
	}
	return s.Created
}
