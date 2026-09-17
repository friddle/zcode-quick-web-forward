//go:build !linux

package browser

// killOrphanedChromium is a no-op off Linux: the orphan reaper walks /proc,
// which only exists there. Windows/macOS rely on the temp-profile cleanup
// path inside the browser process handling instead.
func killOrphanedChromium() {}
