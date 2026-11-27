package domain

// TruncateSHA safely returns the first 8 characters of a SHA hash.
func TruncateSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
