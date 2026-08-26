package protocol

import (
	"reflect"
	"testing"
)

func TestIMAPUIDDifferenceReturnsOnlyNewlyVisibleUIDs(t *testing.T) {
	got := imapUIDDifference([]int{101, 102, 104, 105}, []int{100, 101, 102, 104})
	want := []int{105}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("new UID difference = %v, want %v", got, want)
	}
}

func TestIMAPUIDDifferenceIgnoresUnchangedAndRemovalOnly(t *testing.T) {
	for _, test := range []struct {
		name     string
		current  []int
		previous []int
	}{
		{name: "unchanged", current: []int{11, 12, 13}, previous: []int{11, 12, 13}},
		{name: "removal only", current: []int{11, 13}, previous: []int{11, 12, 13}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := imapUIDDifference(test.current, test.previous); len(got) != 0 {
				t.Fatalf("difference = %v, want no fetch candidates", got)
			}
		})
	}
}

func TestIMAPRecentUIDsUsesSortedTail(t *testing.T) {
	lines := []string{"* SEARCH 9 3 8 4", "A1 OK SEARCH completed"}
	if got, want := imapRecentUIDs(lines, 3), []int{4, 8, 9}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recent UIDs = %v, want %v", got, want)
	}
}
