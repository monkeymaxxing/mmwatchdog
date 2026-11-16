package store

// SetHostNameForTest replaces how the store reads the host name and returns
// the reader it replaced.
func SetHostNameForTest(read func() (string, error)) func() (string, error) {
	previous := hostName
	hostName = read
	return previous
}
