package process

import (
	"reflect"
	"testing"
)

func TestParsePsProcs(t *testing.T) {
	// `ps -axww -o pid,ppid,uid,gid,rss,vsz,state,time,etime,user,command`: a header
	// row (skipped) then three processes. The last has a multi-word command.
	const out = "  PID  PPID   UID   GID    RSS      VSZ STAT      TIME     ELAPSED USER     COMMAND\n" +
		"    1     0     0     0  12000  4300000 Ss     0:03.21    01:23:45 root     /sbin/launchd\n" +
		"  342     1     0     0   8500  4200000 R      0:01.00       42:10 root     /usr/sbin/syslogd\n" +
		" 1099   501   501    20  55000  5000000 S+     1:23.45        00:30 alice    /Apps/Foo.app/Foo --flag a b\n"
	const now = int64(2_000_000_000_000)
	procs := parsePsProcs(out, now)
	if len(procs) != 3 {
		t.Fatalf("got %d procs, want 3", len(procs))
	}

	p := procs[0] // launchd
	if p.Pid != 1 || p.Ppid != 0 || p.Uid != 0 || p.User != "root" {
		t.Errorf("launchd identity = %+v", p)
	}
	if p.Rss != 12000*1024 || p.Vms != 4300000*1024 {
		t.Errorf("launchd mem = rss %d vms %d", p.Rss, p.Vms)
	}
	if p.State != stateS || p.Threads != 1 {
		t.Errorf("launchd state/threads = %d/%d", p.State, p.Threads)
	}
	if p.Name != "launchd" || p.Exe != "/sbin/launchd" || !reflect.DeepEqual(p.Args, []string{"/sbin/launchd"}) {
		t.Errorf("launchd cmd = name=%q exe=%q args=%q", p.Name, p.Exe, p.Args)
	}
	// time 0:03.21 -> 3.21s. etime 01:23:45 -> 5025s -> createTime = now - 5025000.
	if p.UserTime < 3.2 || p.UserTime > 3.22 {
		t.Errorf("launchd UserTime = %v, want ~3.21", p.UserTime)
	}
	if want := now - 5025*1000; p.CreateTime != want {
		t.Errorf("launchd CreateTime = %d, want %d", p.CreateTime, want)
	}

	if procs[1].State != stateR {
		t.Errorf("syslogd state = %d, want running", procs[1].State)
	}
	foo := procs[2] // multi-word command
	if foo.Name != "Foo" || foo.Exe != "/Apps/Foo.app/Foo" ||
		!reflect.DeepEqual(foo.Args, []string{"/Apps/Foo.app/Foo", "--flag", "a", "b"}) {
		t.Errorf("Foo cmd = name=%q exe=%q args=%q", foo.Name, foo.Exe, foo.Args)
	}
}

func TestParsePsThreads(t *testing.T) {
	// `ps -axM`: one line per thread, the first carrying the process columns.
	// launchd shows three threads, Foo two.
	const out = "USER   PID   TT  %CPU STAT PRI    STIME    UTIME COMMAND\n" +
		"root     1   ??   0.0 Ss    31  0:01.00  0:02.00 /sbin/launchd\n" +
		"                  0.0 S     31  0:00.50  0:01.00\n" +
		"                  0.0 S     20  0:00.00  0:00.50\n" +
		"alice 1099   ??   1.0 S     31  0:30.00  0:50.00 /Apps/Foo\n" +
		"                  0.5 S     31  0:10.00  0:20.00\n"
	counts := parsePsThreads(out)
	if counts[1] != 3 {
		t.Errorf("pid 1 threads = %d, want 3", counts[1])
	}
	if counts[1099] != 2 {
		t.Errorf("pid 1099 threads = %d, want 2", counts[1099])
	}
}

func TestPsState(t *testing.T) {
	cases := map[string]ProcState{
		"R": stateR, "R+": stateR, "S": stateS, "Ss": stateS, "I": stateS,
		"T": stateT, "U": stateD, "Z": stateZ, "": stateU,
	}
	for s, want := range cases {
		if got := psState(s); got != want {
			t.Errorf("psState(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestParsePsElapsedSeconds(t *testing.T) {
	cases := map[string]int64{
		"00:30": 30, "42:10": 42*60 + 10, "01:23:45": 5025, "2-03:04:05": 2*86400 + 3*3600 + 4*60 + 5,
	}
	for s, want := range cases {
		if got := parsePsElapsedSeconds(s); got != want {
			t.Errorf("parsePsElapsedSeconds(%q) = %d, want %d", s, got, want)
		}
	}
}
