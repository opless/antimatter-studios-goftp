package goftp

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Listing lines as real servers send them (TREE-1074): GNU and BSD ls, with
// and without a group column, ACL and extended-attribute markers, set-id and
// sticky bits, devices, links, the ISO date styles; and the formats that are
// not supported must be refused by name, never half understood.
func TestParseLISTUnixVariants(t *testing.T) {
	type want struct {
		name, owner, group, target string
		mode                       os.FileMode
		size                       int64
		year                       int // 0: not checked
	}
	for _, c := range []struct {
		line string
		want want
	}{
		{"-rw-r--r--    1 ftp      ftp          1024 Jan 28 12:34 file name.txt", want{name: "file name.txt", owner: "ftp", group: "ftp", mode: 0644, size: 1024}},
		{"drwxr-xr-x    2 1000     1000         4096 Dec 31  2022 dir", want{name: "dir", owner: "1000", group: "1000", mode: os.ModeDir | 0755, size: 4096, year: 2022}},
		{"lrwxrwxrwx    1 root     root            7 Mar  3 10:11 link -> target/x", want{name: "link", owner: "root", group: "root", target: "target/x", mode: os.ModeSymlink | 0777, size: 7}},
		{"-rwsr-xr-x 1 root root 1234 Jan  1  2020 su", want{name: "su", owner: "root", group: "root", mode: os.ModeSetuid | 0755, size: 1234, year: 2020}},
		{"-rwSr--r-- 1 root root 1 Jan  1  2020 setuid-without-x", want{name: "setuid-without-x", owner: "root", group: "root", mode: os.ModeSetuid | 0644, size: 1, year: 2020}},
		{"-rwxr-sr-x 1 root mail 1 Jan  1  2020 setgid", want{name: "setgid", owner: "root", group: "mail", mode: os.ModeSetgid | 0755, size: 1, year: 2020}},
		{"drwxrwxrwt 2 root root 4096 Jan  1 12:00 tmp", want{name: "tmp", owner: "root", group: "root", mode: os.ModeDir | os.ModeSticky | 0777, size: 4096}},
		{"drwxrwxr-T 2 root root 4096 Jan  1 12:00 sticky-no-x", want{name: "sticky-no-x", owner: "root", group: "root", mode: os.ModeDir | os.ModeSticky | 0774, size: 4096}},
		{"-rw-r--r--+ 1 user group 5 Jan  1 12:00 with-acl", want{name: "with-acl", owner: "user", group: "group", mode: 0644, size: 5}},
		{"-rw-r--r--@ 1 user staff 5 Jan  1 12:00 with-xattr", want{name: "with-xattr", owner: "user", group: "staff", mode: 0644, size: 5}},
		{"crw-rw-rw- 1 root root 1, 3 Jan  1 12:00 null", want{name: "null", owner: "root", group: "root", mode: os.ModeDevice | os.ModeCharDevice | 0666}},
		{"prw-r--r-- 1 root root 0 Jan  1 12:00 fifo", want{name: "fifo", owner: "root", group: "root", mode: os.ModeNamedPipe | 0644}},
		{"-rw-r--r-- 1 u g 12 2023-01-28 12:34 long-iso", want{name: "long-iso", owner: "u", group: "g", mode: 0644, size: 12, year: 2023}},
		{"-rw-r--r-- 1 u g 12 2023-01-28 12:34:56.123456789 +0100 full-iso", want{name: "full-iso", owner: "u", group: "g", mode: 0644, size: 12, year: 2023}},
		{"-rw-r--r-- 1 owner 1234 Jan  1  2020 no-group-column", want{name: "no-group-column", owner: "owner", mode: 0644, size: 1234, year: 2020}},
		{"-rw-r--r-- 1 1000 1000 1234 Jan  1  2020 numeric", want{name: "numeric", owner: "1000", group: "1000", mode: 0644, size: 1234, year: 2020}},
		{"-rw-r--r-- 1 u g 12 2 Jan 15:04 day-first", want{name: "day-first", owner: "u", group: "g", mode: 0644, size: 12}},
		{"-rw-r--r-- 1 u g 12 Jan  1  2020   leading spaces in name", want{name: "  leading spaces in name", owner: "u", group: "g", mode: 0644, size: 12, year: 2020}},
	} {
		info, err := parseLIST(c.line, time.UTC, false)
		if err != nil || info == nil {
			t.Errorf("%q: %v", c.line, err)
			continue
		}
		f := info.(*ftpFile)
		w := c.want
		if f.Name() != strings.TrimLeft(w.name, " ") && f.Name() != w.name || f.Owner() != w.owner || f.Group() != w.group || f.LinkTarget() != w.target ||
			f.Mode() != w.mode || (w.size != 0 || w.mode&os.ModeDevice == 0) && f.Size() != w.size {
			t.Errorf("%q:\n got name %q owner %q group %q target %q mode %v size %d\nwant name %q owner %q group %q target %q mode %v size %d",
				c.line, f.Name(), f.Owner(), f.Group(), f.LinkTarget(), f.Mode(), f.Size(), w.name, w.owner, w.group, w.target, w.mode, w.size)
		}
		if w.year != 0 && f.ModTime().Year() != w.year {
			t.Errorf("%q: year %d, want %d", c.line, f.ModTime().Year(), w.year)
		}
	}
}

// A date with a time and no year is in the past: this year, or last year's if that would be in the future.
func TestParseLISTYearlessDates(t *testing.T) {
	now := time.Now()
	later := now.AddDate(0, 0, 40)
	line := "-rw-r--r-- 1 u g 1 " + later.Format("Jan _2 15:04") + " future-looking"
	info, err := parseLIST(line, time.Local, false)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().After(now) {
		t.Errorf("%q gave %v, which is in the future", line, info.ModTime())
	}
	if info.ModTime().Year() != now.Year()-1 && later.Year() == now.Year() {
		t.Errorf("a date 40 days ahead should be last year's, got %v", info.ModTime())
	}
}

func TestParseLISTRefusesUnsupportedFormatsByName(t *testing.T) {
	for _, c := range []struct{ line, format string }{
		{"FILE.TXT;1              4  21-JAN-2023 12:34 [OWNER] (RWED,RWED,RWED,)", "VMS"},
		{"+i8388621.29609,m824255902,/,\tdir", "EPLF"},
	} {
		info, err := parseLIST(c.line, time.UTC, false)
		if err == nil || info != nil || !strings.Contains(err.Error(), c.format) {
			t.Errorf("%q: %v, %v; want a refusal naming %s", c.line, info, err, c.format)
		}
	}
	// junk is an error, not an entry
	if info, err := parseLIST("this is not a listing", time.UTC, false); err == nil || info != nil {
		t.Errorf("junk accepted: %v %v", info, err)
	}
	// "total" lines and blank lines are not entries
	for _, l := range []string{"total 12", "", "   "} {
		if info, err := parseLIST(l, time.UTC, false); err != nil || info != nil {
			t.Errorf("%q: %v %v", l, info, err)
		}
	}
}

// . and .. are left out when asked.
func TestParseLISTSelfAndParent(t *testing.T) {
	for _, n := range []string{".", ".."} {
		line := "drwxr-xr-x 2 u g 4096 Jan  1  2020 " + n
		if info, err := parseLIST(line, time.UTC, true); info != nil || err != nil {
			t.Errorf("%s: %v %v", n, info, err)
		}
		if info, err := parseLIST(line, time.UTC, false); info == nil || err != nil {
			t.Errorf("%s kept: %v %v", n, info, err)
		}
	}
}
