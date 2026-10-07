package source

// ValidMode reports whether a --mode value names a one of the known modes. "auto" and
// "all" are the caller's business, not the registry's.
func ValidMode(mode string) bool {
	if mode == "auto" || mode == "all" {
		return true
	}
	for _, m := range KnownModes {
		if mode == m {
			return true
		}
	}
	return false
}
