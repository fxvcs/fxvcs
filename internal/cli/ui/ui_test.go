package ui

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 18 * 1024: "18.0 KiB", 2411724: "2.3 MiB", 5 << 30: "5.0 GiB"}
	for n, want := range cases {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestPlural(t *testing.T) {
	if got := Plural(1, "asset", "assets"); got != "1 asset" {
		t.Errorf("got %q", got)
	}
	if got := Count(3, "asset"); got != "3 assets" {
		t.Errorf("got %q", got)
	}
	if got := Count(0, "asset"); got != "0 assets" {
		t.Errorf("got %q", got)
	}
}

func TestItemsAlignedWithoutColor(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, Options{})
	p.Header("status", "3 tracked assets")
	p.Blank()
	p.Items([]Item{
		{State: OK, Label: "loaded", Path: "stream/city.ytd", Size: Bytes(2411724)},
		{State: OK, Label: "loaded", Path: "stream/city_props.ytyp", Size: "18.0 KiB", Note: "changed locally"},
		{State: Pending, Label: "placeholder", Path: "stream/bank_int.ymap", Size: "1.4 MiB", Note: "(fxvcs load stream/bank_int.ymap)"},
	})
	p.Footer("1 asset changed locally.")
	want := "fxvcs status — 3 tracked assets\n\n" +
		"  loaded       stream/city.ytd         2.3 MiB\n" +
		"  loaded       stream/city_props.ytyp  18.0 KiB  changed locally\n" +
		"  placeholder  stream/bank_int.ymap    1.4 MiB   (fxvcs load stream/bank_int.ymap)\n" +
		"\n1 asset changed locally.\n"
	if buf.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
	if strings.Contains(buf.String(), "\x1b[") || strings.Contains(buf.String(), "✓") {
		t.Fatal("decoration leaked into non-terminal output")
	}
}

func TestItemsWithColorKeepWords(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, Options{Color: true})
	p.Items([]Item{{State: Fail, Label: "failed", Path: "a.ytd", Note: "permission denied"}})
	out := buf.String()
	if !strings.Contains(out, "✗ failed") || !strings.Contains(out, "\x1b[31m") || !strings.Contains(out, "a.ytd") {
		t.Fatalf("got %q", out)
	}
}

func TestTableAndQuiet(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, Options{})
	p.Table([][]string{{"NAME", "TYPE"}, {"primary", "filesystem"}})
	if got := buf.String(); got != "  NAME     TYPE\n  primary  filesystem\n" {
		t.Fatalf("got %q", got)
	}
	buf.Reset()
	q := New(&buf, Options{Quiet: true})
	q.Header("status", "x")
	q.Items([]Item{{Label: "a"}})
	q.Table([][]string{{"a"}})
	q.Footer("f")
	q.Hint("h")
	q.Line("l")
	if buf.Len() != 0 {
		t.Fatalf("quiet printed %q", buf.String())
	}
}

func TestDetect(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	tty := func(io.Writer) bool { return true }
	notty := func(io.Writer) bool { return false }
	if Detect("always", io.Discard, notty) != true || Detect("never", io.Discard, tty) != false {
		t.Fatal("explicit modes ignored")
	}
	if Detect("auto", io.Discard, tty) != false {
		t.Fatal("NO_COLOR ignored")
	}
	if IsTerminal(io.Discard) {
		t.Fatal("io.Discard is not a terminal")
	}
}

func TestDetectAutoFollowsTerminal(t *testing.T) {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		t.Skip("NO_COLOR set in the environment")
	}
	if Detect("auto", io.Discard, func(io.Writer) bool { return true }) != true {
		t.Fatal("auto on a terminal should be on")
	}
	if Detect("auto", io.Discard, func(io.Writer) bool { return false }) != false {
		t.Fatal("auto off a terminal should be off")
	}
}
