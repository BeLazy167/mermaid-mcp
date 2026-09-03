//go:build linux

package render

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processGroupRSS(processGroupID int) (int64, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	var totalPages int64
	matched := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		closingParen := strings.LastIndexByte(string(stat), ')')
		if closingParen < 0 {
			continue
		}
		fields := strings.Fields(string(stat[closingParen+1:]))
		if len(fields) < 3 {
			continue
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil || group != processGroupID {
			continue
		}
		statm, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "statm"))
		if err != nil {
			continue
		}
		memory := strings.Fields(string(statm))
		if len(memory) < 2 {
			continue
		}
		pages, err := strconv.ParseInt(memory[1], 10, 64)
		if err != nil || pages < 0 {
			continue
		}
		totalPages += pages
		matched = true
	}
	return totalPages * int64(os.Getpagesize()), matched
}
