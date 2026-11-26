package ytdlp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeTool writes a shell script that behaves like yt-dlp for a few video ids.
func fakeTool(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	script := `#!/bin/sh
for a; do url="$a"; done
id="${url##*v=}"
out=""
prev=""
for a; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
echo "$@" > "$PWD/args.txt"
env > "$PWD/env.txt"
file="$(echo "$out" | sed 's/%(ext)s/m4a/')"
case "$id" in
  GoodVideo01) printf 'AUDIO-BYTES' > "$file"; exit 0 ;;
  WebmVideo02) printf 'AUDIO' > "$(echo "$out" | sed 's/%(ext)s/webm/')"; exit 0 ;;
  PartOnly003) printf 'x' > "$file.part"; exit 0 ;;
  BigVideo004) head -c 4096 /dev/zero > "$file"; exit 0 ;;
  Private0005) echo "ERROR: [youtube] Private0005: Private video. Sign in if you've been granted access to this video" >&2; exit 1 ;;
  Age00000006) echo "ERROR: Sign in to confirm your age. This video may be inappropriate for some users." >&2; exit 1 ;;
  Bot00000007) echo "ERROR: Sign in to confirm you're not a bot. Use --cookies-from-browser" >&2; exit 1 ;;
  Gone0000008) echo "ERROR: Video unavailable. This video has been removed by the uploader" >&2; exit 1 ;;
  Nope0000013) echo "ERROR: [youtube] Nope0000013: This video is unavailable" >&2; exit 1 ;;
  Old00000009) echo "ERROR: Requested format is not available. Use --list-formats" >&2; exit 1 ;;
  Long0000010) echo "Long0000010 does not pass filter (duration <= 1200 & !is_live), skipping .." >&2; exit 0 ;;
  Slow0000011) exec sleep 20 ;;
  Weird000012) echo "something odd happened" >&2; exit 1 ;;
esac
exit 1
`
	path := filepath.Join(t.TempDir(), "yt-dlp")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func fetch(t *testing.T, y *YtDlp, id string) (*File, string, error) {
	t.Helper()
	dir := t.TempDir()
	f, err := y.Fetch(context.Background(), id, dir)
	return f, dir, err
}

func TestFetchSuccess(t *testing.T) {
	y := New(fakeTool(t), "node", 1<<20, 20*time.Minute, 10*time.Second)
	f, dir, err := fetch(t, y, "GoodVideo01")
	if err != nil || f.Name != "GoodVideo01.m4a" || f.ContentType != "audio/mp4" || f.Size != 11 {
		t.Fatalf("file = %+v %v", f, err)
	}
	if b, _ := os.ReadFile(f.Path); string(b) != "AUDIO-BYTES" {
		t.Fatalf("content = %q", b)
	}
	f, _, err = fetch(t, y, "WebmVideo02")
	if err != nil || f.ContentType != "audio/webm" {
		t.Fatalf("webm = %+v %v", f, err)
	}

	// how the tool was started
	args, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	a := string(args)
	for _, want := range []string{"--no-playlist", "--ignore-config", "-f bestaudio[ext=m4a]/bestaudio", "--max-filesize 1048576", "duration <= 1200 & !is_live", "--js-runtimes node", "-- https://www.youtube.com/watch?v=GoodVideo01"} {
		if !strings.Contains(a, want) {
			t.Errorf("arguments lack %q: %s", want, a)
		}
	}
	// the tool sees none of our secrets
	t.Setenv("DATABASE_URL", "postgres://secret")
	t.Setenv("JWT_SECRET", "very-secret")
	_, dir2, _ := fetch(t, y, "GoodVideo01")
	env, _ := os.ReadFile(filepath.Join(dir2, "env.txt"))
	if strings.Contains(string(env), "secret") || !strings.Contains(string(env), "HOME="+dir2) {
		t.Fatalf("environment = %s", env)
	}
}

func TestFetchFailures(t *testing.T) {
	y := New(fakeTool(t), "", 1<<20, 20*time.Minute, 10*time.Second)
	for _, c := range []struct {
		id        string
		permanent bool
		contains  string
	}{
		{"Private0005", true, "private"},
		{"Age00000006", true, "age-restricted"},
		{"Gone0000008", true, "unavailable"},
		{"Nope0000013", true, "unavailable"},
		{"Long0000010", true, "longer than a song"},
		{"BigVideo004", false, ""}, // set below: exceeds MaxBytes of this tool instance
		{"Bot00000007", false, "blocking"},
		{"Old00000009", false, "update"},
		{"Weird000012", false, "failed"},
		{"PartOnly003", false, "no audio"},
	} {
		small := *y
		if c.id == "BigVideo004" {
			small.MaxBytes = 1000
			c.permanent, c.contains = true, "too large"
		}
		_, _, err := fetch(t, &small, c.id)
		var fe *Error
		if !errors.As(err, &fe) {
			t.Errorf("%s: err = %v", c.id, err)
			continue
		}
		if fe.Permanent != c.permanent || !strings.Contains(strings.ToLower(fe.Reason), c.contains) {
			t.Errorf("%s: %+v, want permanent=%v reason containing %q", c.id, fe, c.permanent, c.contains)
		}
		if fe.Detail == "" {
			t.Errorf("%s: no detail for the log", c.id)
		}
	}
}

func TestFetchRefusesBadIDsAndMissingTool(t *testing.T) {
	y := New(fakeTool(t), "", 1<<20, time.Minute, 5*time.Second)
	for _, id := range []string{"", "short", "has space 1", "../../etc/pass", "--exec=rm -rf", "GoodVideo01; ls", "GoodVideo011"} {
		if _, _, err := fetch(t, y, id); err == nil {
			t.Errorf("id %q accepted", id)
		} else if fe := new(*Error); !errors.As(err, fe) || !(*fe).Permanent {
			t.Errorf("id %q: %v", id, err)
		}
	}
	missing := New(filepath.Join(t.TempDir(), "nope"), "", 1<<20, time.Minute, 5*time.Second)
	_, _, err := fetch(t, missing, "GoodVideo01")
	var fe *Error
	if !errors.As(err, &fe) || !fe.Permanent || !strings.Contains(fe.Reason, "not available on this server") {
		t.Fatalf("missing tool = %v", err)
	}
}

func TestFetchTimeoutAndCancel(t *testing.T) {
	y := New(fakeTool(t), "", 1<<20, time.Minute, 300*time.Millisecond)
	start := time.Now()
	_, _, err := fetch(t, y, "Slow0000011")
	var fe *Error
	if !errors.As(err, &fe) || fe.Permanent || !strings.Contains(fe.Reason, "too long") || time.Since(start) > 8*time.Second {
		t.Fatalf("timeout = %v after %v", err, time.Since(start))
	}
	// a cancelled job is reported as cancelled, not as a YouTube failure
	y.Timeout = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := y.Fetch(ctx, "Slow0000011", t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
}

func TestValidVideoID(t *testing.T) {
	if !ValidVideoID("dQw4w9WgXcQ") || !ValidVideoID("a-b_c-d_e-f") || ValidVideoID("dQw4w9WgXc") || ValidVideoID("dQw4w9WgXcQQ") || ValidVideoID("dQw4w9 gXcQ") {
		t.Fatal("ValidVideoID")
	}
}
