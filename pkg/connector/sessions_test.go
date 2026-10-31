package connector

import (
	"reflect"
	"testing"

	"pai/pkg/harness"
)

func TestSessionTree(t *testing.T) {
	for _, tt := range []struct {
		name  string
		infos []harness.Info
		ids   []string
		depth []int
	}{
		{name: "empty"},
		{
			name: "parents and siblings keep newest order",
			infos: []harness.Info{
				{ID: "new-child", Parent: "old-parent"},
				{ID: "new-parent"},
				{ID: "orphan", Parent: "missing"},
				{ID: "old-parent"},
				{ID: "old-child", Parent: "old-parent"},
				{ID: "other-child", Parent: "new-parent"},
			},
			ids:   []string{"new-parent", "other-child", "orphan", "old-parent", "new-child", "old-child"},
			depth: []int{0, 1, 0, 0, 1, 1},
		},
		{
			name:  "filtered children stay at top level",
			infos: []harness.Info{{ID: "a", Parent: "missing"}, {ID: "b", Parent: "missing"}},
			ids:   []string{"a", "b"},
			depth: []int{0, 0},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := append([]harness.Info(nil), tt.infos...)
			var ids []string
			var depths []int
			for _, row := range SessionTree(tt.infos) {
				ids = append(ids, row.ID)
				depths = append(depths, row.Depth)
			}
			if !reflect.DeepEqual(ids, tt.ids) || !reflect.DeepEqual(depths, tt.depth) {
				t.Fatalf("got %v at depths %v; want %v at depths %v", ids, depths, tt.ids, tt.depth)
			}
			if !reflect.DeepEqual(tt.infos, before) {
				t.Fatal("input was changed")
			}
		})
	}
}
