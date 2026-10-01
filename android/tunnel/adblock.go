package tunnel

import (
	_ "embed"
	"strings"
	"sync"
	"sync/atomic"
)

// Built by `make adblock-list` (android/adblock/).
//
//go:embed adblock.txt
var adBlockList string

var (
	adBlock     atomic.Bool
	adBlockOnce sync.Once
	adBlockSet  map[string]struct{}
)

// SetAdBlock turns NXDOMAIN for ad domains on or off. The set is built on
// the first turn on, here rather than on the DNS path; never turned on,
// it takes no memory.
func SetAdBlock(on bool) {
	if on {
		adBlockOnce.Do(func() { adBlockSet = parseAdBlock(adBlockList) })
	}
	adBlock.Store(on)
}

func parseAdBlock(list string) map[string]struct{} {
	s := map[string]struct{}{}
	for _, l := range strings.Split(list, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			s[l] = struct{}{}
		}
	}
	return s
}

// blocked: the name or one of its parents is on the list.
func blocked(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for {
		if _, ok := adBlockSet[name]; ok {
			return true
		}
		var more bool
		if _, name, more = strings.Cut(name, "."); !more {
			return false
		}
	}
}
