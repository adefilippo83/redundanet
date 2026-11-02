// Package diskfree reports the space of the filesystem that holds a path,
// for the census heartbeat of a storage device.
package diskfree

// Free returns the bytes available to this process and the filesystem's
// size, for the filesystem that holds path.
func Free(path string) (free, total uint64, err error) { return query(path) }
