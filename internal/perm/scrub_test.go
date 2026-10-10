package perm

import (
	"strings"
	"testing"
)

func TestScrubKeepsPlainShellVariables(t *testing.T) {
	cmd := "JAR=/home/u/.gradle/x.jar && javap -cp $JAR net.minecraft.world.Item"
	if got := Scrub(cmd); got != cmd {
		t.Errorf("plain variable redacted: %q", got)
	}
	for _, secret := range []string{"API_KEY=sk-live-abc def", "export GITHUB_TOKEN=ghp_123456", "db_password=hunter22 rest"} {
		if got := Scrub(secret); !strings.Contains(got, "[REDACTED]") || strings.Contains(got, "abc") || strings.Contains(got, "ghp_") || strings.Contains(got, "hunter22") {
			t.Errorf("%q → %q", secret, got)
		}
	}
}
