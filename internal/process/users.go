//go:build linux || freebsd || openbsd || netbsd || dragonfly

package process

// lookupUser resolves a numeric uid to a user name, caching results for the
// duration of one collection (a host has few distinct uids but many processes).
// On a static, cgo-free build os/user reads /etc/passwd. A uid it cannot resolve
// falls back to its decimal form, which still tags the process meaningfully.
// darwin takes user names straight from ps and never calls this.

import (
	"os/user"
	"strconv"
)

func lookupUser(uid int32, cache map[int32]string) string {
	if name, ok := cache[uid]; ok {
		return name
	}
	name := strconv.Itoa(int(uid))
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	cache[uid] = name
	return name
}
