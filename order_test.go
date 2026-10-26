package main

import (
	"slices"
	"testing"

	"kero/internal/proto"
)

// One sidebar group's display order: the auto base, then pin chains.
func TestOrderSessions(t *testing.T) {
	tests := []struct {
		name  string
		order string
		group []proto.Session
		want  []string
	}{
		{
			name:  "created base is ascending",
			group: []proto.Session{{ID: "c", Created: 300}, {ID: "a", Created: 100}, {ID: "b", Created: 200}},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "created base keeps the given order on ties",
			group: []proto.Session{{ID: "b", Created: 100}, {ID: "a", Created: 100}},
			want:  []string{"b", "a"},
		},
		{
			name:  "empty order means created",
			order: "",
			group: []proto.Session{{ID: "c", Created: 300}, {ID: "a", Created: 100}, {ID: "b", Created: 200}},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "updated base is descending by the fallback key",
			order: "updated",
			group: []proto.Session{
				{ID: "a", Created: 900, Activity: 500},
				{ID: "b", Created: 800, Since: 400},
				{ID: "c", Created: 600},
				{ID: "d", Created: 700},
			},
			want: []string{"d", "c", "a", "b"},
		},
		{
			name: "a pin lifts its session after its anchor",
			group: []proto.Session{
				{ID: "a", Created: 100},
				{ID: "b", Created: 200},
				{ID: "c", Created: 300, Pin: "a"},
				{ID: "d", Created: 400},
			},
			want: []string{"a", "c", "b", "d"},
		},
		{
			name: "a chain follows its head",
			group: []proto.Session{
				{ID: "a", Created: 100},
				{ID: "b", Created: 200},
				{ID: "z", Created: 300},
				{ID: "x", Created: 400, Pin: "b"},
				{ID: "y", Created: 600, Pin: "x"},
			},
			want: []string{"a", "b", "x", "y", "z"},
		},
		{
			name: "two followers of one anchor keep the base order",
			group: []proto.Session{
				{ID: "a", Created: 100},
				{ID: "b", Created: 200},
				{ID: "x", Created: 300, Pin: "b"},
				{ID: "y", Created: 400, Pin: "b"},
				{ID: "z", Created: 500},
			},
			want: []string{"a", "b", "x", "y", "z"},
		},
		{
			name: "pin head stands before the group's first",
			group: []proto.Session{
				{ID: "a", Created: 100},
				{ID: "b", Created: 200},
				{ID: "h", Created: 300, Pin: "head"},
			},
			want: []string{"h", "a", "b"},
		},
		{
			name: "head chains keep the base order",
			group: []proto.Session{
				{ID: "a", Created: 100},
				{ID: "g", Created: 200, Pin: "head"},
				{ID: "b", Created: 300},
				{ID: "h", Created: 400, Pin: "head"},
				{ID: "y", Created: 500, Pin: "g"},
			},
			want: []string{"g", "y", "h", "a", "b"},
		},
		{
			name: "an auto session cannot split a chain",
			group: []proto.Session{
				{ID: "a", Created: 100},
				{ID: "b", Created: 200},
				{ID: "n", Created: 250},
				{ID: "z", Created: 300},
				{ID: "x", Created: 400, Pin: "b"},
				{ID: "y", Created: 600, Pin: "x"},
			},
			want: []string{"a", "b", "x", "y", "n", "z"},
		},
		{
			name: "a deleted anchor returns its follower to auto",
			group: []proto.Session{
				{ID: "a", Created: 100},
				{ID: "b", Created: 200},
				{ID: "x", Created: 400, Pin: "gone"},
			},
			want: []string{"a", "b", "x"},
		},
		{
			name: "a pin into another group is ignored (by date)",
			group: []proto.Session{
				{ID: "a", Created: 100},
				{ID: "b", Created: 200},
				{ID: "x", Created: 400, Pin: "yesterday"},
			},
			want: []string{"a", "b", "x"},
		},
		{
			// x stands between a and b in the auto order, so only a live
			// pin — across projects, as a whole-host group allows — moves
			// it behind b: an ignored pin would leave a, x, b.
			name: "a whole-host group (no projects) keeps pins",
			group: []proto.Session{
				{ID: "a", Project: "p1", Created: 100},
				{ID: "b", Project: "p2", Created: 200},
				{ID: "x", Project: "p1", Created: 150, Pin: "b"},
			},
			want: []string{"a", "b", "x"},
		},
		{
			name:  "pins work over the updated base too",
			order: "updated",
			group: []proto.Session{
				{ID: "a", Created: 100, Activity: 500},
				{ID: "b", Created: 200, Activity: 700},
				{ID: "c", Created: 300, Activity: 200, Pin: "b"},
			},
			want: []string{"b", "c", "a"},
		},
		{
			name: "a pin cycle falls back to auto",
			group: []proto.Session{
				{ID: "a", Created: 100, Pin: "b"},
				{ID: "b", Created: 200, Pin: "a"},
			},
			want: []string{"a", "b"},
		},
		{
			name: "a three-way pin cycle falls back to auto",
			group: []proto.Session{
				{ID: "a", Created: 100, Pin: "b"},
				{ID: "b", Created: 200, Pin: "c"},
				{ID: "c", Created: 300, Pin: "a"},
			},
			want: []string{"a", "b", "c"},
		},
		{
			name: "a self pin falls back to auto",
			group: []proto.Session{
				{ID: "a", Created: 100, Pin: "a"},
				{ID: "b", Created: 200},
			},
			want: []string{"a", "b"},
		},
		{
			name: "a pin onto a cycle member still stands",
			group: []proto.Session{
				{ID: "a", Created: 100, Pin: "b"},
				{ID: "b", Created: 200, Pin: "c"},
				{ID: "c", Created: 300, Pin: "b"},
			},
			want: []string{"b", "a", "c"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := orderSessions(tt.group, tt.order)
			ids := make([]string, len(got))
			for i, s := range got {
				ids[i] = s.ID
			}
			if !slices.Equal(ids, tt.want) {
				t.Errorf("order %q gave %v, want %v", tt.order, ids, tt.want)
			}
		})
	}
}

// The updated key falls back: the program's last write, else the last
// status change, else creation.
func TestOrderUpdatedKey(t *testing.T) {
	tests := []struct {
		name string
		s    proto.Session
		want int64
	}{
		{"the program's last write", proto.Session{ID: "s", Created: 1, Since: 2, Activity: 3}, 3},
		{"else the status's since", proto.Session{ID: "s", Created: 1, Since: 2}, 2},
		{"else creation", proto.Session{ID: "s", Created: 1}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := updatedKey(tt.s); got != tt.want {
				t.Errorf("updatedKey gave %d, want %d", got, tt.want)
			}
		})
	}
}
