package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheLogStaysWithinItsCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "site.log")
	w, err := Open(path, 1000, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// 500 lines of 40 bytes are 20 kB, far more than 3 files of 1 kB hold.
	for i := 0; i < 500; i++ {
		line := fmt.Sprintf("line %04d %s\n", i, strings.Repeat("x", 29))
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}

	paths := Paths(path)
	if len(paths) != 3 || paths[2] != path || paths[0] != path+".2" {
		t.Fatalf("files = %v, want %s.2, %s.1 and the log itself, oldest first", paths, path, path)
	}
	var all string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > 1000 {
			t.Errorf("%s holds %d bytes, want at most 1000", filepath.Base(p), len(data))
		}
		all += string(data)
	}

	// What is kept is the newest lines, whole and in order.
	lines := strings.Split(strings.TrimSuffix(all, "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "line 0499 ") {
		t.Errorf("the last line kept is %q, want line 0499", last)
	}
	first := 500 - len(lines)
	for i, line := range lines {
		if want := fmt.Sprintf("line %04d ", first+i); !strings.HasPrefix(line, want) {
			t.Fatalf("line %d kept is %q, want it to start with %q", i, line, want)
		}
	}
	if len(lines) < 50 {
		t.Errorf("only %d lines were kept, want about 3 kB of them", len(lines))
	}
}

func TestReopeningAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "site.log")
	for _, text := range []string{"one\n", "two\n"} {
		w, err := Open(path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(text))
		w.Close()
	}
	if data, _ := os.ReadFile(path); string(data) != "one\ntwo\n" {
		t.Errorf("the log holds %q", data)
	}
}

func TestASingleFileStartsOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "site.log")
	w, err := Open(path, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 50; i++ {
		w.Write([]byte(fmt.Sprintf("line %04d\n", i)))
	}
	if paths := Paths(path); len(paths) != 1 {
		t.Errorf("files = %v, want only the log itself", paths)
	}
	if info, _ := os.Stat(path); info.Size() > 100 {
		t.Errorf("the log holds %d bytes, want at most 100", info.Size())
	}
}

func TestASmallerCapRemovesWhatIsBeyondIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "site.log")
	for _, name := range []string{path, path + ".1", path + ".2", path + ".3"} {
		os.WriteFile(name, []byte("old\n"), 0o600)
	}
	w, err := Open(path, 1000, 2)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if paths := Paths(path); len(paths) != 2 {
		t.Errorf("files = %v, want the log and one older file", paths)
	}
	if err := Remove(path); err != nil || len(Paths(path)) != 0 {
		t.Errorf("Remove left %v (error %v)", Paths(path), err)
	}
}

func TestParseSize(t *testing.T) {
	for text, want := range map[string]int64{"10MB": 10_000_000, "500 kB": 500_000, "1gb": 1_000_000_000, "2048B": 2048, "1.5MB": 1_500_000} {
		if got, err := ParseSize(text); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	for _, text := range []string{"", "10", "MB", "-1MB", "0kB", "ten MB"} {
		if _, err := ParseSize(text); err == nil {
			t.Errorf("ParseSize(%q) succeeded, want an error", text)
		}
	}
}
