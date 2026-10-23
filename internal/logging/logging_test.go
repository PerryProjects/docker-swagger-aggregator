package logging_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/logging"
)

func TestJSONLoggerHonoursLevelAndFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log, err := logging.New("warn", logging.FormatJSON, &buf)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("dropped: below the level")
	log.Warn("kept", zap.String("service", "orders"), zap.Error(errTest))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one line, got %q", buf.String())
	}
	var rec map[string]any
	if err = json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("not valid JSON: %v (%s)", err, lines[0])
	}
	if rec["level"] != "warn" || rec["msg"] != "kept" || rec["service"] != "orders" || rec["error"] != "boom" ||
		rec["time"] == nil {
		t.Fatalf("unexpected record: %v", rec)
	}
}

func TestConsoleLoggerIsHumanReadable(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log, err := logging.New("debug", logging.FormatConsole, &buf)
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("hello", zap.Int("n", 3))
	if out := buf.String(); !strings.Contains(out, "DEBUG") || !strings.Contains(out, "hello") ||
		!strings.Contains(out, `"n": 3`) {
		t.Fatalf("got %q", out)
	}
}

func TestInvalidSettingsAreRejected(t *testing.T) {
	t.Parallel()

	if _, err := logging.New(
		"loud",
		logging.FormatJSON,
		&bytes.Buffer{},
	); err == nil ||
		!strings.Contains(err.Error(), "loud") {
		t.Errorf("bad level: %v", err)
	}
	if _, err := logging.New("info", "xml", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "xml") {
		t.Errorf("bad format: %v", err)
	}
}

type testError string

func (e testError) Error() string { return string(e) }

const errTest = testError("boom")
