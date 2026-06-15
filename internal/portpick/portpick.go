package portpick

import (
	"fmt"
	"math/rand"
	"net"
	"time"
)

// portRange is an inclusive [Lo, Hi] span of allowed ports.
type portRange struct {
	Lo, Hi int
}

// AllowedRanges are the only ports the agent is permitted to listen on, per
// project policy. The agent picks the first free port from these.
var AllowedRanges = []portRange{
	{19302, 19309},
	{27014, 27050},
}

// Allowed reports whether p falls inside one of the allowed ranges.
func Allowed(p int) bool {
	for _, r := range AllowedRanges {
		if p >= r.Lo && p <= r.Hi {
			return true
		}
	}
	return false
}

// Pick returns a free TCP port from the allowed ranges. It tries ports in a
// shuffled order so repeated installs do not all collide on the same port, and
// verifies each candidate is actually bindable before returning it.
func Pick() (int, error) {
	var candidates []int
	for _, r := range AllowedRanges {
		for p := r.Lo; p <= r.Hi; p++ {
			candidates = append(candidates, p)
		}
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	rng.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})
	for _, p := range candidates {
		if isFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port available in allowed ranges")
}

// isFree reports whether a TCP port can be bound on all interfaces.
func isFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
