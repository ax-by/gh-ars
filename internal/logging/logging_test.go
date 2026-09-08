package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestNew_S11_LevelAndFormat(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "warn", "text")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.Warn("shown", "k", "v")
	out := buf.String()
	if strings.Contains(out, "hidden") || !strings.Contains(out, "msg=shown") || !strings.Contains(out, "k=v") {
		t.Fatalf("text handler at warn: %q", out)
	}

	buf.Reset()
	log, err = New(&buf, "", "")
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("hidden")
	log.Info("shown")
	out = buf.String()
	if strings.Contains(out, "hidden") || !strings.Contains(out, `"msg":"shown"`) {
		t.Fatalf("default json/info: %q", out)
	}

	for _, bad := range [][2]string{{"verbose", "json"}, {"info", "yaml"}} {
		if _, err := New(&buf, bad[0], bad[1]); err == nil {
			t.Errorf("level=%q format=%q: 오류 기대", bad[0], bad[1])
		}
	}
}
