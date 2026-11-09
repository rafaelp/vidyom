package workers

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// totalMemoryMB reports physical memory, zero when unknown.
func totalMemoryMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0
			}
			return kb >> 10
		}
	}
	return 0
}
