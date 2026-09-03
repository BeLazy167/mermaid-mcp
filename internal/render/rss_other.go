//go:build !linux

package render

func processGroupRSS(int) (int64, bool) { return 0, false }
