package cli

import "testing"

func TestApplyWebUIEnvOverridesFromEnv(t *testing.T) {
	t.Setenv("OFELIA_UI", "true")
	t.Setenv("OFELIA_UI_BIND", "9090")
	t.Setenv("OFELIA_UI_REFRESH_SEC", "7")

	cmd := &DaemonCommand{
		WebUI:           false,
		WebUIBind:       defaultWebUIBindAddress,
		WebUIRefreshSec: defaultWebUIRefresh,
	}

	cmd.applyWebUIEnvOverrides()

	if !cmd.WebUI {
		t.Fatalf("expected WebUI to be enabled from env")
	}
	if cmd.WebUIBind != ":9090" {
		t.Fatalf("expected WebUIBind to be normalized to :9090, got %q", cmd.WebUIBind)
	}
	if cmd.WebUIRefreshSec != 7 {
		t.Fatalf("expected WebUIRefreshSec to be 7, got %d", cmd.WebUIRefreshSec)
	}
}

func TestNormalizeWebUIBind(t *testing.T) {
	testCases := []struct {
		name     string
		in       string
		expected string
	}{
		{name: "numeric port", in: "8080", expected: ":8080"},
		{name: "already bound", in: ":8080", expected: ":8080"},
		{name: "host and port", in: "0.0.0.0:8080", expected: "0.0.0.0:8080"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeWebUIBind(tc.in)
			if got != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestApplyWebUIEnvOverridesKeepsExplicitCLIValues(t *testing.T) {
	t.Setenv("OFELIA_UI", "false")
	t.Setenv("OFELIA_UI_BIND", ":7777")
	t.Setenv("OFELIA_UI_REFRESH_SEC", "11")

	cmd := &DaemonCommand{
		WebUI:           true,
		WebUIBind:       ":8088",
		WebUIRefreshSec: 3,
	}

	cmd.applyWebUIEnvOverrides()

	if !cmd.WebUI {
		t.Fatalf("expected WebUI to remain enabled from explicit CLI value")
	}
	if cmd.WebUIBind != ":8088" {
		t.Fatalf("expected WebUIBind to remain :8088, got %q", cmd.WebUIBind)
	}
	if cmd.WebUIRefreshSec != 3 {
		t.Fatalf("expected WebUIRefreshSec to remain 3, got %d", cmd.WebUIRefreshSec)
	}
}

func TestApplyWebUIEnvOverridesInvalidRefreshIgnored(t *testing.T) {
	t.Setenv("OFELIA_UI_REFRESH_SEC", "bad")

	cmd := &DaemonCommand{
		WebUIBind:       defaultWebUIBindAddress,
		WebUIRefreshSec: defaultWebUIRefresh,
	}

	cmd.applyWebUIEnvOverrides()

	if cmd.WebUIRefreshSec != defaultWebUIRefresh {
		t.Fatalf("expected invalid env refresh to be ignored, got %d", cmd.WebUIRefreshSec)
	}
}
