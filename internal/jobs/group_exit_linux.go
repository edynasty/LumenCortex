//go:build linux

package jobs

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// SIGKILL delivery and the leader's Wait do not prove siblings have exited.
// Observe the original group becoming quiescent before publishing cancellation.
func waitStoppedGroup(pgid int) error {
	deadline := time.Now().Add(killGrace)
	for {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return err
		}
		live := false
		for _, entry := range entries {
			if _, err := strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			b, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
			if err != nil {
				continue
			}
			end := strings.LastIndexByte(string(b), ')')
			if end < 0 {
				continue
			}
			fields := strings.Fields(string(b[end+1:]))
			if len(fields) < 3 {
				continue
			}
			group, _ := strconv.Atoi(fields[2])
			if group == pgid && fields[0] != "Z" && fields[0] != "X" {
				live = true
				break
			}
		}
		if !live {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("stopped job group %d still has live members", pgid)
		}
		time.Sleep(time.Millisecond)
	}
}
