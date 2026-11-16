package index

import "sort"

func sortBy[T any](list []T, less func(a, b T) bool) {
	sort.SliceStable(list, func(i, j int) bool { return less(list[i], list[j]) })
}
